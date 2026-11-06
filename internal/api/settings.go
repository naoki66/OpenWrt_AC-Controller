package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/naoki66/ac-controller/internal/model"
	"github.com/naoki66/ac-controller/internal/store"
)

// Settings endpoints back the runtime knobs an operator turns from the devices
// page. The management API already exposes PSKs in clear text, so echoing the
// effective shared secret back to a token-holding caller leaks nothing new.

type sharedSecretView struct {
	// Env is the AC_SHARED_SECRET default the process started with.
	Env string `json:"env"`
	// Override is the value set through this API, "" when none.
	Override string `json:"override"`
	// Effective is what the AP channel enforces right now.
	Effective string `json:"effective"`
	// Source names where Effective comes from: "settings" or "env".
	Source string `json:"source"`
}

func (s *Server) sharedSecretView() sharedSecretView {
	v := sharedSecretView{Env: s.SharedSecret}
	s.secretMu.RLock()
	v.Override = s.secretOverride
	s.secretMu.RUnlock()
	v.Effective = s.EffectiveSecret()
	if v.Override != "" {
		v.Source = "settings"
	} else {
		v.Source = "env"
	}
	return v
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"shared_secret": s.sharedSecretView(),
		"ntp":           s.EffectiveNTPServers(),
	})
}

// handleSetNTP replaces the site time-source list. An empty list removes the
// override and falls back to AC_NTP_SERVERS; APs keep their last applied list
// either way, so an AC outage never stops the NTP client.
func (s *Server) handleSetNTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servers []string `json:"servers"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	servers := model.NormalizeNTPServers(req.Servers)

	if len(servers) == 0 {
		if err := s.Store.DeleteSetting(r.Context(), store.SettingNTPServers); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.SetNTPOverride(nil)
		s.Store.LogEvent(r.Context(), "settings", "", "NTP server override removed; "+
			"falling back to AC_NTP_SERVERS")
	} else {
		raw, err := json.Marshal(servers)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := s.Store.SetSetting(r.Context(), store.SettingNTPServers, string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.SetNTPOverride(servers)
		s.Store.LogEvent(r.Context(), "settings", "",
			"NTP servers set from the system page: "+strings.Join(servers, ","))
	}

	writeJSON(w, http.StatusOK, map[string]any{"ntp": s.EffectiveNTPServers()})
}

// handleSetSharedSecret rotates the AP shared secret at runtime.
//
// Rotation is immediate: the next hello from an AP still carrying the old
// secret is rejected, and the next hello with the new one succeeds. Adopted
// devices are unaffected until they re-authenticate, so the operator must
// update /etc/config/ac on every AP before (or right after) rotating — the UI
// says so.
//
// An empty secret removes the override and falls back to the environment
// default; it does not disable authentication unless the environment value is
// empty too.
func (s *Server) handleSetSharedSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Secret string `json:"secret"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}

	secret := strings.TrimSpace(req.Secret)
	if secret != "" {
		if len(secret) < 8 {
			writeError(w, http.StatusBadRequest, errors.New("secret must be at least 8 characters"))
			return
		}
		if s.AdminToken != "" && secret == s.AdminToken {
			writeError(w, http.StatusBadRequest, errors.New(
				"secret must differ from the admin token: APs knowing it could read the management API"))
			return
		}
	}

	if secret == "" {
		if err := s.Store.DeleteSetting(r.Context(), store.SettingSharedSecret); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.SetSecretOverride("")
		s.Store.LogEvent(r.Context(), "settings", "", "shared secret override removed; "+
			"falling back to environment value")
	} else {
		if err := s.Store.SetSetting(r.Context(), store.SettingSharedSecret, secret); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.SetSecretOverride(secret)
		s.Store.LogEvent(r.Context(), "settings", "", "AP shared secret rotated from the devices page")
	}

	writeJSON(w, http.StatusOK, map[string]any{"shared_secret": s.sharedSecretView()})
}
