package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/model"
)

// Settings endpoints back the runtime knobs an operator turns from the devices
// page.
//
// The shared secret is returned in clear text. It is the one value an operator
// has to retype onto every AP by hand (/etc/config/ac), and a masked hint makes
// that impossible — the operator would have to dig the value back out of the
// compose file. The trade-off is deliberate: this endpoint sits behind the same
// admin session that can already rotate the secret, and a secret equal to the
// admin token is rejected, so reading it grants nothing that writing it did not
// already grant.

type sharedSecretView struct {
	// Source names where the effective secret comes from: "settings" or "env".
	Source string `json:"source"`
	// Set reports whether any effective secret exists at all.
	Set bool `json:"set"`
	// Value is the effective secret in clear text; empty when none is set.
	// The devices page shows it so it can be copied straight onto an AP.
	Value string `json:"value"`
}

func (s *Server) sharedSecretView() sharedSecretView {
	s.secretMu.RLock()
	override := s.secretOverride
	s.secretMu.RUnlock()
	eff := s.EffectiveSecret()
	v := sharedSecretView{
		Set:   eff != "",
		Value: eff,
	}
	if override != "" {
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
		if len(secret) < 12 {
			writeError(w, http.StatusBadRequest, errors.New("secret must be at least 12 characters"))
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
