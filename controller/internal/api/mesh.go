package api

import (
	"encoding/json"
	"net/http"

	"github.com/naoki66/ac-controller/shared/model"
)

func (s *Server) handleTemplateMesh(w http.ResponseWriter, r *http.Request) {
	// 局部更新只替换回程配置，保留模板中的无线服务、射频和网桥设置。
	var req struct {
		Mesh *model.MeshBackhaul `json:"mesh"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, err)
		return
	}
	t, err := s.Store.GetTemplate(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	t.Mesh = req.Mesh
	if err := t.Validate(); err != nil {
		writeError(w, 400, err)
		return
	}
	if err := s.Store.UpsertTemplate(r.Context(), t); err != nil {
		writeError(w, 500, err)
		return
	}
	s.Store.LogEvent(r.Context(), "mesh", "", "template "+t.ID+" backhaul updated")
	writeJSON(w, 200, t)
}

func (s *Server) handleMesh(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.LatestMesh(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"nodes": list})
}
