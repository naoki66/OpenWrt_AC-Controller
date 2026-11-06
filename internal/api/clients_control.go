package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/naoki66/ac-controller/internal/discovery"
	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// handleSetClientMeta stores operator intent about one station: alias, note,
// tracking flag and the "smart magnet" SSID/AP preference. It is a partial
// update, so the client detail form can send only what changed.
func (s *Server) handleSetClientMeta(w http.ResponseWriter, r *http.Request) {
	mac := discovery.NormalizeMAC(r.PathValue("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return
	}

	var req struct {
		Alias     *string `json:"alias"`
		Note      *string `json:"note"`
		Tracked   *bool   `json:"tracked"`
		BindSSID  *string `json:"bind_ssid"`
		BindAPMAC *string `json:"bind_ap_mac"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	meta, err := s.Store.GetClientMeta(r.Context(), mac)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if req.Alias != nil {
		meta.Alias = strings.TrimSpace(*req.Alias)
		if len(meta.Alias) > 64 {
			writeError(w, http.StatusBadRequest, errors.New("alias must be at most 64 bytes"))
			return
		}
	}
	if req.Note != nil {
		meta.Note = strings.TrimSpace(*req.Note)
		if len(meta.Note) > 512 {
			writeError(w, http.StatusBadRequest, errors.New("note must be at most 512 bytes"))
			return
		}
	}
	if req.Tracked != nil {
		meta.Tracked = *req.Tracked
	}
	if req.BindSSID != nil {
		meta.BindSSID = strings.TrimSpace(*req.BindSSID)
		if len(meta.BindSSID) > 32 {
			writeError(w, http.StatusBadRequest, errors.New("bind_ssid must be at most 32 bytes"))
			return
		}
	}
	if req.BindAPMAC != nil {
		v := strings.TrimSpace(*req.BindAPMAC)
		if v == "" {
			meta.BindAPMAC = ""
		} else {
			meta.BindAPMAC = discovery.NormalizeMAC(v)
			if meta.BindAPMAC == "" {
				writeError(w, http.StatusBadRequest, errors.New("malformed bind_ap_mac"))
				return
			}
		}
	}
	// An AP preference without an SSID has no meaning: the agent steers by
	// SSID first and only narrows to one AP afterwards.
	if meta.BindSSID == "" {
		meta.BindAPMAC = ""
	}
	meta.MAC = mac

	if err := s.Store.UpsertClientMeta(r.Context(), meta); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "client_meta", mac, "alias="+meta.Alias)
	writeJSON(w, http.StatusOK, meta)
}

// handleKickClient deauthenticates one client from the AP it is associated
// with. It queues the same kind of action as the device page, so the result is
// reported back on the AP's next hello rather than blocking this request.
func (s *Server) handleKickClient(w http.ResponseWriter, r *http.Request) {
	mac := discovery.NormalizeMAC(r.PathValue("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return
	}
	client, err := s.Store.GetClient(r.Context(), mac)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, errors.New("client not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !client.Online {
		writeError(w, http.StatusConflict, errors.New("client is offline"))
		return
	}
	if client.APMAC == "" || client.Ifname == "" {
		writeError(w, http.StatusConflict, errors.New("client association is incomplete"))
		return
	}
	device, err := s.Store.GetDevice(r.Context(), client.APMAC)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if device.State != store.StateAdopted {
		writeError(w, http.StatusConflict, errors.New("AP is not adopted"))
		return
	}

	a, err := s.Store.CreateAction(r.Context(), client.APMAC, proto.ActionKick, map[string]string{
		"mac":    mac,
		"ifname": client.Ifname,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "client_kick", mac, client.APMAC+"/"+client.Ifname)
	writeJSON(w, http.StatusAccepted, a)
}
