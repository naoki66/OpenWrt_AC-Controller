package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/naoki66/ac-controller/internal/store"
)

// groupRequest is the body of create/update. Both fields are always sent by
// the UI; an empty template_id clears the binding intentionally.
type groupRequest struct {
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Store.ListGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeGroupRequest(w, r)
	if !ok {
		return
	}
	if !s.groupTemplateExists(w, r, req.TemplateID) {
		return
	}

	g, err := s.Store.CreateGroup(r.Context(), req.Name, req.TemplateID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "group_create", "", g.Name+" ("+g.ID+") template="+g.TemplateID)
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, ok := decodeGroupRequest(w, r)
	if !ok {
		return
	}
	if !s.groupTemplateExists(w, r, req.TemplateID) {
		return
	}
	if _, err := s.Store.GetGroup(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.Store.UpdateGroup(r.Context(), id, req.Name, req.TemplateID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "group_update", "", req.Name+" ("+id+") template="+req.TemplateID)
	g, err := s.Store.GetGroup(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// handleDeleteGroup refuses to delete a group that still has members. Detaching
// devices would leave them on a template nobody chose, and the operator almost
// certainly meant to remove an empty group.
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.Store.CountGroupDevices(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict,
			fmt.Errorf("分组内仍有 %d 台设备，请先解绑后再删除", n))
		return
	}
	if err := s.Store.DeleteGroup(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "group_delete", "", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetDeviceGroup binds (or unbinds, with an empty group_id) one device.
//
// When the target group carries a template and the device is already adopted,
// the new template is rendered and becomes the next revision immediately: a
// group binding that only records intent would report success while the AP
// keeps running the previous configuration indefinitely.
func (s *Server) handleSetDeviceGroup(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	req.GroupID = strings.TrimSpace(req.GroupID)

	device, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	templateID := ""
	if req.GroupID != "" {
		g, err := s.Store.GetGroup(r.Context(), req.GroupID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, errors.New("分组不存在"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		templateID = g.TemplateID
	}

	if err := s.Store.SetDeviceGroup(r.Context(), mac, req.GroupID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "group_bind", mac, req.GroupID)

	if device.State == store.StateAdopted && templateID != "" && templateID != device.TemplateID {
		if _, err := s.Provision.AssignTemplate(r.Context(), mac, templateID); err != nil {
			// Membership is already recorded; report the push failure so the
			// operator knows the AP still runs the old revision.
			writeError(w, http.StatusBadGateway,
				fmt.Errorf("分组已绑定，但模板下发失败：%w", err))
			return
		}
	}

	d, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// groupTemplateExists validates an optional template reference before it is
// written. An unknown template would otherwise fail much later, at the first
// package generation, far from the action that caused it.
func (s *Server) groupTemplateExists(w http.ResponseWriter, r *http.Request, templateID string) bool {
	if templateID == "" {
		return true
	}
	_, err := s.Store.GetTemplate(r.Context(), templateID)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("模板 %q 不存在", templateID))
		return false
	}
	writeError(w, http.StatusInternalServerError, err)
	return false
}

func decodeGroupRequest(w http.ResponseWriter, r *http.Request) (groupRequest, bool) {
	var req groupRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return groupRequest{}, false
	}
	req.Name = strings.TrimSpace(req.Name)
	req.TemplateID = strings.TrimSpace(req.TemplateID)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("分组名称不能为空"))
		return groupRequest{}, false
	}
	if len(req.Name) > 64 {
		writeError(w, http.StatusBadRequest, errors.New("分组名称最长 64 个字符"))
		return groupRequest{}, false
	}
	return req, true
}

// adoptTemplateFor returns the template a device should use when the caller did
// not name one: its group's binding first, otherwise the seeded default.
func (s *Server) adoptTemplateFor(r *http.Request, mac string) string {
	if d, err := s.Store.GetDevice(r.Context(), mac); err == nil && d.GroupID != "" {
		if g, err := s.Store.GetGroup(r.Context(), d.GroupID); err == nil && g.TemplateID != "" {
			return g.TemplateID
		}
	}
	return "default"
}
