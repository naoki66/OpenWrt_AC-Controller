package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/naoki66/ac-controller/controller/internal/discovery"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/secret"
)

const (
	deviceChallengeTTL = 45 * time.Second
	deviceChallengeLen = 8 + 24 + sha256.Size
	maxUsedChallenges  = 16384
)

type deviceCredentials struct {
	mac, key, challenge string
	enrollmentID        string
	enrolled            bool
}
type deviceAuthContextKey struct{}

func (s *Server) challengeMAC(body []byte) []byte {
	// A restart invalidates every outstanding challenge as well as the in-RAM
	// replay cache. Never reuse the controller's long-lived signing key here.
	s.challengeOnce.Do(func() { _, _ = rand.Read(s.challengeKey[:]) })
	mac := hmac.New(sha256.New, s.challengeKey[:])
	mac.Write([]byte("ac-controller/device-challenge/v1\x00"))
	mac.Write(body)
	return mac.Sum(nil)
}

func (s *Server) handleDeviceChallenge(w http.ResponseWriter, _ *http.Request) {
	var challenge [deviceChallengeLen]byte
	binary.BigEndian.PutUint64(challenge[:], uint64(s.now().Unix()))
	if _, err := rand.Read(challenge[8:32]); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("could not generate challenge"))
		return
	}
	copy(challenge[32:], s.challengeMAC(challenge[:32]))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(hex.EncodeToString(challenge[:])))
}

// verifyDevice authenticates the exact request and consumes a short lived
// controller challenge. The AP's own clock is not needed during boot.
func (s *Server) verifyDevice(r *http.Request, body []byte) (deviceCredentials, bool) {
	mac := discovery.NormalizeMAC(r.Header.Get(secret.DeviceMACHeader))
	if mac == "" {
		return deviceCredentials{}, false
	}
	public, enrollmentID, err := s.Store.EnrollmentCredentials(r.Context(), mac)
	if err != nil {
		return deviceCredentials{}, false
	}
	key := secret.DeviceKey(s.EffectiveSecret(), mac)
	if enrollmentID != "" {
		// Authentication and adoption are separate. A released AP can still
		// authenticate a heartbeat, but it cannot fetch configuration.
		deploymentKey, err := s.Store.GetSetting(r.Context(), store.SettingManagementKey)
		if err != nil {
			return deviceCredentials{}, false
		}
		exchange, err := s.enrollmentDeviceKey(mac, public)
		if err != nil {
			return deviceCredentials{}, false
		}
		key = secret.ManagementTrafficKey(deploymentKey, exchange, enrollmentID)
	} else if public != "" {
		key, err = s.enrollmentDeviceKey(mac, public)
		if err != nil {
			return deviceCredentials{}, false
		}
	}
	cred, ok := s.verifyDeviceKey(r, body, mac, key)
	cred.enrolled = public != ""
	cred.enrollmentID = enrollmentID
	return cred, ok
}

func (s *Server) verifyDeviceKey(r *http.Request, body []byte, mac, key string) (deviceCredentials, bool) {
	challenge := r.Header.Get(secret.DeviceChallengeHeader)
	var raw [deviceChallengeLen]byte
	if mac == "" || len(challenge) != hex.EncodedLen(len(raw)) {
		return deviceCredentials{}, false
	}
	if _, err := hex.Decode(raw[:], []byte(challenge)); err != nil ||
		!hmac.Equal(raw[32:], s.challengeMAC(raw[:32])) {
		return deviceCredentials{}, false
	}
	now := s.now()
	issued := time.Unix(int64(binary.BigEndian.Uint64(raw[:8])), 0)
	expiry := issued.Add(deviceChallengeTTL)
	if issued.After(now) || !expiry.After(now) {
		return deviceCredentials{}, false
	}
	if key == "" {
		return deviceCredentials{}, false
	}
	proof := r.Header.Get(secret.DeviceProofHeader)
	want := secret.DeviceProof(key, mac, challenge, r.Method, r.URL.RequestURI(), body)
	if !hmac.Equal([]byte(proof), []byte(want)) {
		return deviceCredentials{}, false
	}
	s.deviceAuthMu.Lock()
	defer s.deviceAuthMu.Unlock()
	if s.usedChallenges == nil {
		s.usedChallenges = make(map[[deviceChallengeLen]byte]time.Time)
	}
	if _, used := s.usedChallenges[raw]; used {
		return deviceCredentials{}, false
	}
	if !now.Before(s.sweepChallengesAt) {
		for token, expires := range s.usedChallenges {
			if !expires.After(now) {
				delete(s.usedChallenges, token)
			}
		}
		s.sweepChallengesAt = now.Add(5 * time.Second)
	}
	if len(s.usedChallenges) >= maxUsedChallenges {
		return deviceCredentials{}, false
	}
	s.usedChallenges[raw] = expiry
	return deviceCredentials{mac: mac, key: key, challenge: challenge}, true
}

func withDevice(r *http.Request, cred deviceCredentials) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), deviceAuthContextKey{}, cred))
}

func deviceFrom(r *http.Request) (deviceCredentials, bool) {
	cred, ok := r.Context().Value(deviceAuthContextKey{}).(deviceCredentials)
	return cred, ok
}
