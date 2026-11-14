package api

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/naoki66/ac-controller/controller/internal/discovery"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/secret"
)

func (s *Server) handleEnrollmentInfo(w http.ResponseWriter, _ *http.Request) {
	key, err := secret.EnrollmentKey(s.Key.Private)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	public := hex.EncodeToString(key.PublicKey().Bytes())
	sig := ed25519.Sign(s.Key.Private, secret.EnrollmentStatement(s.ACID, public))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, proto.EnrollmentInfo{ACID: s.ACID, Public: public, Signature: hex.EncodeToString(sig), SigningKey: s.Key.PublicHex()})
}

func (s *Server) enrollmentDeviceKey(mac, device string) (string, error) {
	peer, err := secret.ParseEnrollmentPublic(device)
	if err != nil {
		return "", err
	}
	key, err := secret.EnrollmentKey(s.Key.Private)
	if err != nil {
		return "", err
	}
	return secret.EnrollmentDeviceKey(key, peer, mac, hex.EncodeToString(key.PublicKey().Bytes()), device)
}

func (s *Server) handleEnrollment(w http.ResponseWriter, r *http.Request) {
	control := http.NewResponseController(w)
	if err := control.SetReadDeadline(time.Now().Add(5 * time.Second)); err == nil {
		defer control.SetReadDeadline(time.Time{})
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid enrollment request size"))
		return
	}
	mac := discovery.NormalizeMAC(r.Header.Get(secret.DeviceMACHeader))
	var info proto.Discover
	if mac == "" || json.Unmarshal(body, &info) != nil || discovery.NormalizeMAC(info.MAC) != mac {
		writeError(w, http.StatusBadRequest, errors.New("invalid enrollment identity"))
		return
	}
	if info.EnrollmentID != "" {
		if raw, err := hex.DecodeString(info.EnrollmentID); err != nil || len(raw) != 16 {
			writeError(w, http.StatusBadRequest, errors.New("invalid enrollment ID"))
			return
		}
	}
	public := r.Header.Get(secret.EnrollmentPublicHeader)
	if (info.PublicKey != "" && info.PublicKey != public) ||
		info.ResetGeneration < 0 || (info.ResetGeneration != 0 && info.EnrollmentID == "") {
		writeError(w, http.StatusBadRequest, errors.New("invalid enrollment metadata"))
		return
	}
	key, err := s.enrollmentDeviceKey(mac, public)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid device enrollment key"))
		return
	}
	cred, ok := s.verifyDeviceKey(r, body, mac, key)
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("device enrollment proof failed"))
		return
	}
	ok, err = s.Store.RegisterEnrollment(r.Context(), store.Device{
		MAC: mac, Model: info.Model, Firmware: info.Firmware, AgentVersion: info.AgentVersion,
		Hostname: info.Hostname, IP: clientIP(r),
	}, public, info.EnrollmentID, info.ResetGeneration)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, errors.New("MAC already registered to another device identity or newer enrollment"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if info.EnrollmentID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	managementKey, err := s.Store.ClaimManagementKey(r.Context(), mac, info.EnrollmentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if managementKey == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload, err := json.Marshal(proto.ManagementClaim{ACID: s.ACID, SigningKey: s.Key.PublicHex(), Credential: managementKey})
	if err == nil {
		payload, err = secret.SealDevicePackage(key, cred.challenge, payload)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", secret.DevicePackageType)
	_, _ = w.Write(payload)
}
