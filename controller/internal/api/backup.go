package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/naoki66/ac-controller/controller/internal/discovery"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/model"
)

// Configuration export/import is a JSON snapshot of the controller-side intent
// that is safe to move between instances: templates, AP groups and per-device
// overrides. It deliberately excludes:
//
//   - adoption state and revisions — those describe a running AP, not policy;
//   - firmware images — potentially hundreds of megabytes;
//   - action history and events — audit, not configuration;
//   - the shared secret — a credential, not a setting.
//
// Import uses upsert-by-ID semantics: an object whose ID already exists is
// replaced, anything else on the controller is left alone. Nothing is deleted,
// and no AP is re-pushed implicitly; an operator reviews the fleet and pushes
// when ready.

const (
	acConfigFormat  = "ac-controller-config"
	acConfigVersion = 1
	// A snapshot holds templates with SSIDs and device overrides; a few
	// megabytes is already an unusually large fleet. The cap keeps a malformed
	// upload from exhausting memory before validation can reject it.
	maxConfigBytes = 32 << 20
)

type configGroup struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TemplateID string `json:"template_id,omitempty"`
}

type configDeviceOverride struct {
	MAC     string                      `json:"mac"`
	GroupID string                      `json:"group_id,omitempty"`
	Network *model.DeviceNetwork        `json:"network,omitempty"`
	Radios  *model.DeviceRadioOverrides `json:"radios,omitempty"`
}

type configSnapshot struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	ExportedAt int64  `json:"exported_at,omitempty"`
	ACID       string `json:"ac_id,omitempty"`

	Templates       []model.Template       `json:"templates"`
	Groups          []configGroup          `json:"groups"`
	DeviceOverrides []configDeviceOverride `json:"device_overrides"`
}

type configImportResult struct {
	Templates      int      `json:"templates"`
	Groups         int      `json:"groups"`
	Devices        int      `json:"devices"`
	SkippedDevices []string `json:"skipped_devices,omitempty"`
}

func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	templates, err := s.Store.ListTemplates(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	groups, err := s.Store.ListGroups(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	devices, err := s.Store.ListDevices(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	now := s.now()
	snap := configSnapshot{
		Format:          acConfigFormat,
		Version:         acConfigVersion,
		ExportedAt:      now.Unix(),
		ACID:            s.ACID,
		Templates:       templates,
		Groups:          make([]configGroup, 0, len(groups)),
		DeviceOverrides: make([]configDeviceOverride, 0, len(devices)),
	}
	for _, g := range groups {
		snap.Groups = append(snap.Groups, configGroup{ID: g.ID, Name: g.Name, TemplateID: g.TemplateID})
	}
	for _, d := range devices {
		if d.GroupID == "" && d.NetworkParams == "" && d.RadioOverrides == "" {
			continue
		}
		ov := configDeviceOverride{MAC: d.MAC, GroupID: d.GroupID}
		if d.NetworkParams != "" {
			var network model.DeviceNetwork
			if err := json.Unmarshal([]byte(d.NetworkParams), &network); err == nil {
				ov.Network = &network
			}
			// A row written by an older controller may not decode into today's
			// struct. Dropping the unreadable override is better than exporting
			// something the importing controller would apply blind.
		}
		if d.RadioOverrides != "" {
			var radios model.DeviceRadioOverrides
			if err := json.Unmarshal([]byte(d.RadioOverrides), &radios); err == nil {
				ov.Radios = &radios
			}
		}
		snap.DeviceOverrides = append(snap.DeviceOverrides, ov)
	}

	name := "ac-config-" + now.Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap)
}

func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var snap configSnapshot
	if err := dec.Decode(&snap); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid configuration snapshot: %w", err))
		return
	}
	if err := s.validateConfigSnapshot(r, &snap); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	res, err := s.applyConfigSnapshot(r, &snap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "config_import", "",
		fmt.Sprintf("imported %d templates, %d groups, %d device overrides",
			res.Templates, res.Groups, res.Devices))
	writeJSON(w, http.StatusOK, res)
}

// validateConfigSnapshot checks the whole document before anything is written,
// so a bad entry does not leave the controller half-imported.
func (s *Server) validateConfigSnapshot(r *http.Request, snap *configSnapshot) error {
	if snap.Format != acConfigFormat {
		return fmt.Errorf("format must be %q", acConfigFormat)
	}
	if snap.Version != acConfigVersion {
		return fmt.Errorf("unsupported version %d (this controller accepts %d)", snap.Version, acConfigVersion)
	}

	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	seenTemplate := map[string]bool{}
	for i, t := range snap.Templates {
		if strings.TrimSpace(t.ID) == "" {
			add("templates[%d]: id is required", i)
			continue
		}
		if seenTemplate[t.ID] {
			add("templates[%d]: id %q is duplicated", i, t.ID)
			continue
		}
		seenTemplate[t.ID] = true
		if err := t.Validate(); err != nil {
			add("templates[%d] %q: %v", i, t.ID, err)
		}
	}

	// Groups may reference a template from the snapshot or one that already
	// exists on this controller; both are valid targets.
	knownTemplate := map[string]bool{}
	if existing, err := s.Store.ListTemplates(r.Context()); err == nil {
		for _, t := range existing {
			knownTemplate[t.ID] = true
		}
	}
	for id := range seenTemplate {
		knownTemplate[id] = true
	}

	seenGroup := map[string]bool{}
	for i, g := range snap.Groups {
		if strings.TrimSpace(g.ID) == "" {
			add("groups[%d]: id is required", i)
			continue
		}
		if seenGroup[g.ID] {
			add("groups[%d]: id %q is duplicated", i, g.ID)
			continue
		}
		seenGroup[g.ID] = true
		if strings.TrimSpace(g.Name) == "" {
			add("groups[%d] %q: name is required", i, g.ID)
		}
		if g.TemplateID != "" && !knownTemplate[g.TemplateID] {
			add("groups[%d] %q: template %q does not exist in the snapshot or on this controller",
				i, g.ID, g.TemplateID)
		}
	}

	knownGroup := map[string]bool{}
	if existing, err := s.Store.ListGroups(r.Context()); err == nil {
		for _, g := range existing {
			knownGroup[g.ID] = true
		}
	}
	for id := range seenGroup {
		knownGroup[id] = true
	}

	seenMAC := map[string]bool{}
	for i, ov := range snap.DeviceOverrides {
		mac := discovery.NormalizeMAC(ov.MAC)
		if mac == "" {
			add("device_overrides[%d]: mac %q is malformed", i, ov.MAC)
			continue
		}
		if seenMAC[mac] {
			add("device_overrides[%d]: mac %q is duplicated", i, mac)
			continue
		}
		seenMAC[mac] = true
		if ov.GroupID != "" && !knownGroup[ov.GroupID] {
			add("device_overrides[%d] %s: group %q does not exist in the snapshot or on this controller",
				i, mac, ov.GroupID)
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// applyConfigSnapshot writes the validated document. Templates first, then
// groups, then device membership: each layer may reference the one before it.
func (s *Server) applyConfigSnapshot(r *http.Request, snap *configSnapshot) (configImportResult, error) {
	ctx := r.Context()
	var res configImportResult

	for _, t := range snap.Templates {
		if err := s.Store.UpsertTemplate(ctx, t); err != nil {
			return res, err
		}
		res.Templates++
	}
	for _, g := range snap.Groups {
		if err := s.Store.UpsertGroup(ctx, store.APGroup{ID: g.ID, Name: g.Name, TemplateID: g.TemplateID}); err != nil {
			return res, err
		}
		res.Groups++
	}

	devices, err := s.Store.ListDevices(ctx)
	if err != nil {
		return res, err
	}
	known := map[string]bool{}
	for _, d := range devices {
		known[d.MAC] = true
	}
	for _, ov := range snap.DeviceOverrides {
		mac := discovery.NormalizeMAC(ov.MAC)
		if !known[mac] {
			// A snapshot restored before the APs have been discovered must not
			// fail the whole import; the override simply waits with the device.
			res.SkippedDevices = append(res.SkippedDevices, mac)
			continue
		}
		if err := s.Store.SetDeviceGroup(ctx, mac, ov.GroupID); err != nil {
			return res, err
		}
		stored := ""
		if ov.Network != nil && !ov.Network.IsZero() {
			blob, err := json.Marshal(*ov.Network)
			if err != nil {
				return res, err
			}
			stored = string(blob)
		}
		if err := s.Store.SetDeviceNetwork(ctx, mac, stored); err != nil {
			return res, err
		}
		radios := ""
		if ov.Radios != nil && !ov.Radios.IsZero() {
			blob, err := json.Marshal(*ov.Radios)
			if err != nil {
				return res, err
			}
			radios = string(blob)
		}
		if err := s.Store.SetDeviceRadios(ctx, mac, radios); err != nil {
			return res, err
		}
		res.Devices++
	}
	return res, nil
}
