package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// maxFirmwareBytes caps an upload. An XR1710G sysupgrade image is in the tens
// of MB; the limit exists so a misdirected upload cannot exhaust the NAS.
const maxFirmwareBytes = 300 << 20

// handleFirmwares lists the repository without the image bytes.
func (s *Server) handleFirmwares(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListFirmwares(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if list == nil {
		list = []store.Firmware{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"firmwares": list})
}

// handleUploadFirmware stores one image.
//
// The body is the image itself rather than multipart: an AP-side or scripted
// upload then needs no form parser, and the digest is computed over exactly the
// bytes that were sent. model/version/filename travel in the query string.
func (s *Server) handleUploadFirmware(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	model := strings.TrimSpace(q.Get("model"))
	version := strings.TrimSpace(q.Get("version"))
	if model == "" || version == "" {
		writeError(w, http.StatusBadRequest, errors.New("model and version are required"))
		return
	}
	if strings.ContainsAny(version, `/\`) {
		// It ends up in a Content-Disposition filename; a slash is not a version.
		writeError(w, http.StatusBadRequest, errors.New("version must not contain a path separator"))
		return
	}
	filename := strings.TrimSpace(q.Get("filename"))
	if filename == "" {
		filename = version + ".img"
	}

	body := http.MaxBytesReader(w, r.Body, maxFirmwareBytes)
	blob, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("read upload: %w", err))
		return
	}
	if len(blob) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("empty firmware image"))
		return
	}

	sum := sha256.Sum256(blob)
	fw := store.Firmware{
		Model:    model,
		Version:  version,
		Filename: filename,
		Size:     int64(len(blob)),
		SHA256:   hex.EncodeToString(sum[:]),
	}
	if err := s.Store.SaveFirmware(r.Context(), fw, blob); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, fmt.Errorf("firmware %s %s already exists", model, version))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "firmware", "", fmt.Sprintf("uploaded %s %s (%d bytes)", model, version, len(blob)))

	// Echo the stored metadata: uploaded_at is assigned by the store, and the
	// digest is the value the operator must see before pushing it to an AP.
	stored, _, err := s.Store.GetFirmware(r.Context(), model, version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, stored)
}

// handleDeleteFirmware removes one image from the repository.
func (s *Server) handleDeleteFirmware(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if model == "" || version == "" {
		writeError(w, http.StatusBadRequest, errors.New("model and version are required"))
		return
	}
	if err := s.Store.DeleteFirmware(r.Context(), model, version); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "firmware", "", "deleted "+model+" "+version)
	w.WriteHeader(http.StatusNoContent)
}

// handleFirmwareImage serves the exact bytes for one build. Both sides use it:
// an AP pulls with its shared secret, an operator pulls with the admin token.
func (s *Server) handleFirmwareImage(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	version := strings.TrimSpace(r.URL.Query().Get("version"))
	if model == "" || version == "" {
		writeError(w, http.StatusBadRequest, errors.New("model and version are required"))
		return
	}

	fw, blob, err := s.Store.GetFirmware(r.Context(), model, version)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	name := fw.Filename
	if name == "" {
		name = version + ".img"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	w.Header().Set("X-Ac-Sha256", fw.SHA256)
	w.Header().Set("ETag", `"`+fw.SHA256+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// handleSetFirmwareTarget records which build one AP should run. It does not
// push anything by itself: the next hello carries the instruction, so an offline
// AP picks it up when it comes back.
func (s *Server) handleSetFirmwareTarget(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}

	var req struct {
		Version string `json:"version"`
	}
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
	}
	version := strings.TrimSpace(req.Version)

	device, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	if version != "" {
		if device.Model == "" {
			writeError(w, http.StatusUnprocessableEntity, errors.New("device reports no model; cannot match a firmware image"))
			return
		}
		if _, _, err := s.Store.GetFirmware(r.Context(), device.Model, version); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest,
					fmt.Errorf("no firmware %s %s in the repository", device.Model, version))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := s.Store.SetFirmwareTarget(r.Context(), mac, version); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "firmware", mac, "target set to "+version)

	updated, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// firmwareRef builds the instruction the agent receives in a hello reply.
//
// It is sent only while the device is adopted, has a target, and has not already
// reported that target as applied or failed. Re-sending after a failure would
// put a device into a reboot loop over an image it cannot boot.
func (s *Server) firmwareRef(r *http.Request, d store.Device) *proto.FirmwareRef {
	if d.State != store.StateAdopted || d.FwTarget == "" {
		return nil
	}
	switch d.FWState {
	case proto.FirmwareApplied, proto.FirmwareFailed:
		return nil
	}
	fw, _, err := s.Store.GetFirmware(r.Context(), d.Model, d.FwTarget)
	if err != nil {
		return nil
	}
	return &proto.FirmwareRef{
		Model:   fw.Model,
		Version: fw.Version,
		Path: "/api/v1/firmwares/image?model=" + url.QueryEscape(fw.Model) +
			"&version=" + url.QueryEscape(fw.Version),
		SHA256: fw.SHA256,
		Size:   fw.Size,
	}
}
