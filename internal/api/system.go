package api

import (
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/naoki66/ac-controller/internal/store"
	"github.com/naoki66/ac-controller/internal/version"
)

var errInvalidLimit = errors.New("limit must be 1-500")

// System endpoints answer "what is this controller and what does it know".
// They deliberately contain no secrets: the effective shared secret is
// reported as a boolean, not as its value. The device page already offers the
// value to a token-holding caller; the system page should stay safe to paste
// into a ticket.

type systemDeviceCounts struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Pending int `json:"pending"`
	Adopted int `json:"adopted"`
}

type systemClientCounts struct {
	Total  int `json:"total"`
	Online int `json:"online"`
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	devices, err := s.Store.ListDevices(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.annotatePresence(devices)

	clients, err := s.Store.ListClients(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	clientSamples, radioSamples, err := s.Store.CountSamples(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	events, err := s.Store.CountEvents(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var dc systemDeviceCounts
	for _, d := range devices {
		dc.Total++
		if d.Online {
			dc.Online++
		}
		if d.State == store.StateAdopted {
			dc.Adopted++
		} else {
			dc.Pending++
		}
	}
	var cc systemClientCounts
	for _, c := range clients {
		cc.Total++
		if c.Online {
			cc.Online++
		}
	}

	sampleSeconds := int64(0)
	if s.SampleInterval > 0 {
		sampleSeconds = int64(s.SampleInterval / time.Second)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":    version.Version,
		"commit":     version.Commit,
		"ac_id":      s.ACID,
		"go_version": runtime.Version(),
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		// The management surface always requires a login account; the AP channel
		// is only protected when a shared secret is in force.
		"admin_auth_required": true,
		"ap_auth_required":    s.EffectiveSecret() != "",
		// Zero means every heartbeat is stored; the agent's cadence is the
		// throttle instead of a controller-side timer.
		"sample_interval_seconds": sampleSeconds,
		"offline_after_seconds":   int64(s.offlineAfter() / time.Second),
		"devices":                 dc,
		"clients":                 cc,
		"telemetry": map[string]int64{
			"client_samples": clientSamples,
			"radio_samples":  radioSamples,
		},
		"events": map[string]int{"total": events},
	})
}

// handleEvents lists the newest audit rows. It is the controller's own log;
// per-AP system/kernel logs come back through the action channel instead, so
// they stay on the AP until an operator explicitly asks for them.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, errInvalidLimit)
			return
		}
		limit = n
	}
	list, err := s.Store.ListEvents(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	total, err := s.Store.CountEvents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "total": total})
}
