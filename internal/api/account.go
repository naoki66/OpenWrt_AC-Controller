package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/internal/store"
)

// The management surface is protected by a login account. The default exists so
// a freshly deployed controller is usable immediately — open the page, log in
// as admin/123456, change the password. The hash lives in the settings table, so
// the change survives restarts and container recreation.
const (
	defaultAdminUsername = "admin"
	defaultAdminPassword = "123456"
	// minAdminPasswordLen is short on purpose: the seeded default is six
	// characters, so demanding eight would make the very first change fail.
	minAdminPasswordLen = 6

	sessionCookieName = "ac_session"
	sessionTTL        = 24 * time.Hour

	// pbkdf2Iterations is cheap enough for a NAS CPU (tens of milliseconds per
	// login) while keeping offline guessing of a leaked database expensive.
	pbkdf2Iterations = 120000
	pbkdf2KeyLen     = 32
)

// EnsureAdminAccount seeds the login account on first start. It returns true
// when a default was written, which is what the startup log warns about.
func EnsureAdminAccount(ctx context.Context, st *store.Store) (bool, error) {
	seeded := false

	if _, err := st.GetSetting(ctx, store.SettingAdminUser); errors.Is(err, store.ErrNotFound) {
		if err := st.SetSetting(ctx, store.SettingAdminUser, defaultAdminUsername); err != nil {
			return false, err
		}
		seeded = true
	} else if err != nil {
		return false, err
	}

	if _, err := st.GetSetting(ctx, store.SettingAdminPass); errors.Is(err, store.ErrNotFound) {
		hash, err := hashPassword(defaultAdminPassword)
		if err != nil {
			return false, err
		}
		if err := st.SetSetting(ctx, store.SettingAdminPass, hash); err != nil {
			return false, err
		}
		seeded = true
	} else if err != nil {
		return false, err
	}

	return seeded, nil
}

// adminAccount reads the stored credentials. A missing row means startup did
// not seed them, which is a deployment error rather than an auth failure.
func (s *Server) adminAccount(ctx context.Context) (user, hash string, err error) {
	user, err = s.Store.GetSetting(ctx, store.SettingAdminUser)
	if err != nil {
		return "", "", fmt.Errorf("load admin account: %w", err)
	}
	hash, err = s.Store.GetSetting(ctx, store.SettingAdminPass)
	if err != nil {
		return "", "", fmt.Errorf("load admin account: %w", err)
	}
	return user, hash, nil
}

// isDefaultPassword reports whether the stored hash still matches the seeded
// default, so the UI can keep asking the operator to change it.
func (s *Server) isDefaultPassword(ctx context.Context) bool {
	_, hash, err := s.adminAccount(ctx)
	return err == nil && verifyPassword(hash, defaultAdminPassword)
}

// --- sessions ---------------------------------------------------------------

type session struct {
	username string
	expires  time.Time
}

// sessionStore keeps session tokens in memory. A restart logs everyone out,
// which is the right recovery for a controller that rarely restarts.
type sessionStore struct {
	mu      sync.Mutex
	byToken map[string]session
}

func (ss *sessionStore) create(username string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.byToken == nil {
		ss.byToken = map[string]session{}
	}
	for t, s := range ss.byToken {
		if now.After(s.expires) {
			delete(ss.byToken, t)
		}
	}
	ss.byToken[token] = session{username: username, expires: now.Add(sessionTTL)}
	return token, nil
}

func (ss *sessionStore) lookup(token string) (session, bool) {
	if token == "" {
		return session{}, false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.byToken[token]
	if !ok {
		return session{}, false
	}
	if time.Now().After(s.expires) {
		delete(ss.byToken, token)
		return session{}, false
	}
	return s, true
}

func (ss *sessionStore) revoke(token string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.byToken, token)
}

// revokeUser drops every session of one account; used after a password change
// so a stolen cookie stops working.
func (ss *sessionStore) revokeUser(username string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for t, s := range ss.byToken {
		if s.username == username {
			delete(ss.byToken, t)
		}
	}
}

// fromRequest returns the session a request carries, if any.
func (s *Server) sessionFrom(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return session{}, false
	}
	return s.sessions.lookup(c.Value)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		// Lax still keeps the cookie off cross-site POSTs, which is the CSRF
		// surface that matters here, while surviving normal navigations.
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// --- handlers ---------------------------------------------------------------

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin exchanges a username and password for a session cookie.
//
// Failed attempts consume the same per-address budget the token scheme uses, so
// a password search is throttled; a correct credential is checked first, so an
// operator sharing an address with a guesser is not locked out.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := remoteIP(r)

	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}

	user, hash, err := s.adminAccount(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The credential is checked before the budget, mirroring the Bearer path:
	// an operator sharing an address with a guesser is never locked out, and a
	// success clears the failures recorded for that address. Verify the password
	// even when the name is wrong: short-circuiting would make the PBKDF2 cost a
	// timing oracle for the account name.
	nameOK := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Username)), []byte(user)) == 1
	passOK := verifyPassword(hash, req.Password)
	if !nameOK || !passOK {
		if wait, ok := s.auth.blocked(ip); ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			writeError(w, http.StatusTooManyRequests,
				fmt.Errorf("too many failed authentication attempts; try again in %s", wait.Round(time.Second)))
			return
		}
		if wait, locked := s.auth.fail(ip); locked {
			s.Store.LogEvent(r.Context(), "auth", "", "login locked out for "+ip)
			if s.Log != nil {
				s.Log.Warn("login failures locked out", "ip", ip)
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			writeError(w, http.StatusTooManyRequests,
				fmt.Errorf("too many failed authentication attempts; try again in %s", wait.Round(time.Second)))
			return
		}
		writeError(w, http.StatusUnauthorized, errors.New("用户名或密码错误"))
		return
	}

	s.auth.reset(ip)
	token, err := s.sessions.create(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	setSessionCookie(w, r, token)
	s.Store.LogEvent(r.Context(), "auth", "", "login succeeded from "+ip)

	writeJSON(w, http.StatusOK, map[string]any{
		// authenticated mirrors /auth/session: the UI applies the same session
		// object to both the login response and the startup probe.
		"authenticated":    true,
		"username":         user,
		"default_password": verifyPassword(hash, defaultAdminPassword),
	})
}

// handleSession tells the UI whether the cookie it holds is still valid, and
// whether the account still uses the seeded default password. It stays public:
// the page has to load before it can log in.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sess, ok := s.sessionFrom(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":  false,
			"login_required": true,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":    true,
		"username":         sess.username,
		"default_password": s.isDefaultPassword(r.Context()),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.revoke(c.Value)
	}
	clearSessionCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type accountChangeRequest struct {
	Username        string `json:"username"`
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangeAccount updates the login name and/or password. The current
// password is always required; a new password may be left empty when only the
// name changes. Every other session is dropped and the caller gets a fresh
// cookie, so a stolen cookie stops working — and a renamed account cannot be
// used by a session issued under the old name.
func (s *Server) handleChangeAccount(w http.ResponseWriter, r *http.Request) {
	var req accountChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}

	user, hash, err := s.adminAccount(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !verifyPassword(hash, req.CurrentPassword) {
		writeError(w, http.StatusUnauthorized, errors.New("当前密码不正确"))
		return
	}

	newName := strings.TrimSpace(req.Username)
	if newName == "" {
		newName = user
	}
	if len(newName) > 32 || strings.ContainsAny(newName, " \t\r\n/") {
		writeError(w, http.StatusBadRequest, errors.New("用户名不能超过 32 个字符，且不能包含空白或斜杠"))
		return
	}
	rename := newName != user

	// Passwords are used verbatim: trimming here would silently change a
	// password that was deliberately typed with a leading or trailing space.
	newPassword := req.NewPassword
	if newPassword != "" && len(newPassword) < minAdminPasswordLen {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("新密码至少 %d 个字符", minAdminPasswordLen))
		return
	}
	rekey := newPassword != ""

	if !rename && !rekey {
		writeError(w, http.StatusBadRequest, errors.New("没有需要修改的内容"))
		return
	}

	if rename {
		if err := s.Store.SetSetting(r.Context(), store.SettingAdminUser, newName); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if rekey {
		newHash, err := hashPassword(newPassword)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := s.Store.SetSetting(r.Context(), store.SettingAdminPass, newHash); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	s.sessions.revokeUser(user)
	token, err := s.sessions.create(newName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	setSessionCookie(w, r, token)

	msg := "admin account changed"
	switch {
	case rename && rekey:
		msg = "admin account name and password changed"
	case rename:
		msg = "admin account renamed to " + newName
	default:
		msg = "admin account password changed"
	}
	s.Store.LogEvent(r.Context(), "auth", "", msg)

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":    true,
		"username":         newName,
		"default_password": s.isDefaultPassword(r.Context()),
	})
}

// --- password hashing -------------------------------------------------------

// hashPassword produces "pbkdf2-sha256$<iter>$<salt>$<key>", all base64 raw
// so the value is safe inside the settings table and JSON.
func hashPassword(plain string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2SHA256([]byte(plain), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(stored, plain string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 10*pbkdf2Iterations {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(plain), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2SHA256 is RFC 8018 PBKDF2 with HMAC-SHA256. Implemented here rather
// than pulling in golang.org/x/crypto so the controller keeps its single
// dependency (the sqlite driver).
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	out := make([]byte, 0, blocks*hashLen)
	blockBuf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(blockBuf, uint32(block))

		prf.Reset()
		prf.Write(salt)
		prf.Write(blockBuf)
		sum := prf.Sum(nil)

		t := make([]byte, hashLen)
		copy(t, sum)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(sum)
			sum = prf.Sum(nil)
			for j := range t {
				t[j] ^= sum[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
