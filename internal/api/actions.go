package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/naoki66/ac-controller/internal/discovery"
	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// allowedActions is the closed set the controller may queue. Unknown types are
// rejected at the API boundary rather than relying on the agent to ignore them:
// an operator who typo'd a command should see the error immediately.
var allowedActions = map[string]bool{
	proto.ActionReboot:       true,
	proto.ActionWifiReload:   true,
	proto.ActionFactoryReset: true,
	proto.ActionKick:         true,
	proto.ActionDiagnose:     true,
	proto.ActionLEDSet:       true,
}

// diagnoseKinds is the closed set the agent supports. Validating here means a
// typo is rejected immediately instead of costing a heartbeat round trip.
var diagnoseKinds = map[string]bool{
	proto.DiagnoseNetwork: true,
	proto.DiagnoseSystem:  true,
	proto.DiagnoseKernel:  true,
}

// ifnameRe is the shape of a netdev the agent may pass to ubus. It is checked
// at both edges so a MAC that somehow reached the API cannot become a new
// command line.
var ifnameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// handleDeviceAction queues one command for an AP. It returns 202: the action is
// accepted, not executed — the AP picks it up on its next hello.
func (s *Server) handleDeviceAction(w http.ResponseWriter, r *http.Request) {
	mac := discovery.NormalizeMAC(r.PathValue("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return
	}
	device, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, errors.New("device not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if device.State != store.StateAdopted {
		writeError(w, http.StatusConflict, errors.New("device is not adopted"))
		return
	}

	var req struct {
		Type string            `json:"type"`
		Args map[string]string `json:"args,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !allowedActions[req.Type] {
		writeError(w, http.StatusBadRequest, errors.New("unsupported action type: "+req.Type))
		return
	}
	if req.Type == proto.ActionKick {
		if discovery.NormalizeMAC(req.Args["mac"]) == "" {
			writeError(w, http.StatusBadRequest, errors.New("kick_client needs a valid args.mac"))
			return
		}
		if !ifnameRe.MatchString(req.Args["ifname"]) {
			writeError(w, http.StatusBadRequest, errors.New("kick_client needs a valid args.ifname"))
			return
		}
	}
	if req.Type == proto.ActionDiagnose {
		if !diagnoseKinds[req.Args["kind"]] {
			writeError(w, http.StatusBadRequest,
				errors.New("diagnose needs args.kind: network, system or kernel"))
			return
		}
	}
	if req.Type == proto.ActionLEDSet {
		state := strings.ToLower(strings.TrimSpace(req.Args["state"]))
		if state != "on" && state != "off" {
			writeError(w, http.StatusBadRequest, errors.New("led_set needs args.state: on or off"))
			return
		}
		if req.Args == nil {
			req.Args = map[string]string{}
		}
		req.Args["state"] = state
	}

	a, err := s.Store.CreateAction(r.Context(), mac, req.Type, req.Args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "action_requested", mac, a.Type+" "+a.ID)
	writeJSON(w, http.StatusAccepted, a)
}

// handleDeviceActions lists recent commands and their results for one AP.
func (s *Server) handleDeviceActions(w http.ResponseWriter, r *http.Request) {
	mac := discovery.NormalizeMAC(r.PathValue("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be 1-100"))
			return
		}
		limit = n
	}
	list, err := s.Store.ListActions(r.Context(), mac, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": list})
}
