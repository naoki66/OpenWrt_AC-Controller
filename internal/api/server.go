package api

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naoki66/ac-controller/internal/discovery"
	"github.com/naoki66/ac-controller/internal/keys"
	"github.com/naoki66/ac-controller/internal/model"
	"github.com/naoki66/ac-controller/internal/provision"
	"github.com/naoki66/ac-controller/internal/store"
	"github.com/naoki66/ac-controller/internal/version"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	Store     *store.Store
	ACID      string
	Key       keys.Key
	Provision *provision.Service
	// SharedSecret guards the AP-facing endpoint. Empty disables the check,
	// which is only acceptable on a trusted management segment. It is the
	// environment default; a value set through PUT /api/v1/settings takes
	// precedence (see secretOverride) so an operator can rotate it without
	// recreating the container.
	SharedSecret string
	// AdminToken is an optional Bearer credential for automation. The Web UI no
	// longer uses it: operators log in with the account seeded by
	// EnsureAdminAccount, and the session cookie is accepted here as well.
	AdminToken string
	// SampleInterval throttles how often a hello may append telemetry rows.
	// Heartbeats run every 20s; storing a row per station that often costs
	// ~40k rows a day for 30 clients and buys nothing a chart can show.
	SampleInterval time.Duration
	// OfflineAfter is how long an AP may go without a heartbeat before it is
	// reported offline. Zero means DefaultOfflineAfter.
	OfflineAfter time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
	Log *slog.Logger

	// lastSample records when each AP last contributed telemetry rows, keyed by
	// MAC. It is intentionally not persisted: a restart simply lets the next
	// hello through, which is exactly the right recovery.
	lastSample map[string]time.Time
	sampleMu   sync.Mutex

	// auth tracks failed credential attempts per source address so a guessed
	// token or password cannot be hammered at network speed. The zero value is
	// usable; a restart simply clears every lockout.
	auth authTracker

	// sessions holds the login sessions issued by POST /api/v1/auth/login. The
	// zero value is usable; a restart logs everyone out.
	sessions sessionStore

	// secretOverride mirrors the shared_secret row in the settings table. It
	// is read on every AP request, hence the lock; writes are one-per-click.
	secretMu       sync.RWMutex
	secretOverride string

	// NTPServers is the environment default for site time sources. An override
	// set from the system page wins, exactly like the shared secret.
	NTPServers  []string
	ntpMu       sync.RWMutex
	ntpOverride []string
}

// SetNTPOverride seeds the runtime NTP list. Nil means "no override", in which
// case the AC_NTP_SERVERS default applies.
func (s *Server) SetNTPOverride(v []string) {
	s.ntpMu.Lock()
	s.ntpOverride = append([]string(nil), v...)
	s.ntpMu.Unlock()
}

// EffectiveNTPServers returns a copy of the list handed to APs.
func (s *Server) EffectiveNTPServers() []string {
	s.ntpMu.RLock()
	if len(s.ntpOverride) > 0 {
		out := append([]string(nil), s.ntpOverride...)
		s.ntpMu.RUnlock()
		return out
	}
	s.ntpMu.RUnlock()
	return append([]string(nil), s.NTPServers...)
}

// SetSecretOverride seeds the runtime override. Call once during startup with
// the value persisted in the settings table ("" means no override).
func (s *Server) SetSecretOverride(v string) {
	s.secretMu.Lock()
	s.secretOverride = v
	s.secretMu.Unlock()
}

// EffectiveSecret returns the AP shared secret actually in force: the UI-set
// override when present, otherwise the environment default.
func (s *Server) EffectiveSecret() string {
	s.secretMu.RLock()
	defer s.secretMu.RUnlock()
	if s.secretOverride != "" {
		return s.secretOverride
	}
	return s.SharedSecret
}

// sampleDue reports whether this hello may write telemetry. Throttling lives
// here rather than in the agent so the cadence can be changed without
// re-provisioning every AP.
func (s *Server) sampleDue(mac string) bool {
	if s.SampleInterval <= 0 {
		return true
	}
	now := time.Now()
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	last, ok := s.lastSample[mac]
	if ok && now.Sub(last) < s.SampleInterval {
		return false
	}
	if s.lastSample == nil {
		s.lastSample = map[string]time.Time{}
	}
	s.lastSample[mac] = now
	return true
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/ac/pubkey", s.handlePubKey)
	mux.HandleFunc("POST /api/v1/ap/hello", s.handleHello)

	// Login itself has to be reachable without a session; the session probe is
	// how the UI decides whether to show the login form.
	mux.HandleFunc("GET /api/v1/auth/session", s.handleSession)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	// Everything below is the management surface: it exposes templates, which
	// carry PSKs in clear text, and lets a caller rewrite network configuration.
	// It requires a logged-in session, or AC_ADMIN_TOKEN for automation.
	admin := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, s.adminOnly(h))
	}

	admin("GET /api/v1/devices", s.handleDevices)
	admin("GET /api/v1/devices/{mac}", s.handleDevice)
	admin("POST /api/v1/devices/{mac}/adopt", s.handleAdopt)
	admin("POST /api/v1/devices/{mac}/repush", s.handleRepush)
	admin("PUT /api/v1/devices/{mac}/network", s.handleSetDeviceNetwork)
	admin("PUT /api/v1/devices/{mac}/radios", s.handleSetDeviceRadios)
	admin("PUT /api/v1/devices/{mac}/group", s.handleSetDeviceGroup)
	admin("POST /api/v1/devices/{mac}/release", s.handleRelease)
	admin("GET /api/v1/devices/{mac}/packages", s.handlePackages)
	admin("POST /api/v1/devices/{mac}/firmware", s.handleSetFirmwareTarget)

	// Firmware repository. Upload/list/delete are management operations; the
	// image itself is pulled by the AP with its shared secret as well.
	admin("GET /api/v1/firmwares", s.handleFirmwares)
	admin("POST /api/v1/firmwares", s.handleUploadFirmware)
	admin("DELETE /api/v1/firmwares", s.handleDeleteFirmware)

	admin("GET /api/v1/clients", s.handleClients)
	admin("PUT /api/v1/clients/{mac}", s.handleSetClientMeta)
	admin("POST /api/v1/clients/{mac}/kick", s.handleKickClient)

	// One-shot commands (reboot, reload wireless). Results come back on the
	// AP's next hello and are kept for the device page.
	admin("POST /api/v1/devices/{mac}/actions", s.handleDeviceAction)
	admin("GET /api/v1/devices/{mac}/actions", s.handleDeviceActions)
	admin("PUT /api/v1/devices/{mac}/reboot-schedule", s.handleSetRebootSchedule)
	admin("PUT /api/v1/devices/{mac}/led-schedule", s.handleSetLEDSchedule)

	admin("GET /api/v1/radios", s.handleRadios)
	admin("GET /api/v1/ports", s.handlePorts)
	admin("GET /api/v1/telemetry/clients", s.handleTelemetryClients)
	admin("GET /api/v1/telemetry/radios", s.handleTelemetryRadios)
	admin("GET /api/v1/telemetry/counts", s.handleTelemetryCounts)

	// System operations (P5): controller diagnostics, the audit log and
	// configuration snapshots.
	admin("GET /api/v1/diagnostics", s.handleDiagnostics)
	admin("GET /api/v1/events", s.handleEvents)
	admin("GET /api/v1/config/export", s.handleConfigExport)
	admin("POST /api/v1/config/import", s.handleConfigImport)

	admin("GET /api/v1/templates", s.handleTemplates)
	admin("POST /api/v1/templates", s.handleCreateTemplate)
	admin("GET /api/v1/templates/{id}", s.handleTemplate)
	admin("DELETE /api/v1/templates/{id}", s.handleDeleteTemplate)
	admin("POST /api/v1/templates/{id}/clone", s.handleCloneTemplate)
	// Partial updates: the SSID editor must not rewrite the layer-2 fabric it
	// does not display, and renaming must not require re-sending 18 SSIDs.
	admin("PUT /api/v1/templates/{id}/ssids", s.handleTemplateSSIDs)
	admin("PUT /api/v1/templates/{id}/name", s.handleTemplateRename)

	// AP groups: membership plus the template each group's members should run.
	admin("GET /api/v1/groups", s.handleGroups)
	admin("POST /api/v1/groups", s.handleCreateGroup)
	admin("PUT /api/v1/groups/{id}", s.handleUpdateGroup)
	admin("DELETE /api/v1/groups/{id}", s.handleDeleteGroup)

	// Runtime settings (AP shared secret rotation from the devices page).
	admin("GET /api/v1/settings", s.handleSettings)
	admin("PUT /api/v1/settings/shared-secret", s.handleSetSharedSecret)
	admin("PUT /api/v1/settings/ntp", s.handleSetNTP)

	// Account management: the login name and password are what the UI rotates
	// from the system page.
	admin("PUT /api/v1/auth/account", s.handleChangeAccount)

	// The package download is the one endpoint both sides use: the AP pulls its
	// own bundle with the shared secret, an operator pulls one to inspect it.
	// Either credential is enough; neither alone may read another device's
	// bundle without matching its own identity.
	mux.HandleFunc("GET /api/v1/devices/{mac}/package", s.adminOrAP(s.handlePackage))
	mux.HandleFunc("GET /api/v1/firmwares/image", s.adminOrAP(s.handleFirmwareImage))

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // embed path is a compile-time constant
	}
	// The UI itself stays reachable without a token: it holds no data of its
	// own, and the page has to load before it can ask for one.
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return logRequests(s.Log, mux)
}

// adminOnly rejects a request unless it carries a logged-in session or the
// configured admin token.
//
// Login is always required now, unlike the old token-only scheme which stayed
// open when AC_ADMIN_TOKEN was empty: the account is seeded on first start
// (admin/123456), so there is always something to log in with.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Management responses carry PSKs and device topology; a caching proxy
		// between the browser and the controller must not keep them.
		w.Header().Set("Cache-Control", "no-store")
		if !s.authorizedAdmin(r) {
			s.rejectAuth(w, r, "login required")
			return
		}
		s.auth.reset(remoteIP(r))
		next(w, r)
	}
}

// adminOrAP accepts either credential. Used for the endpoint an AP needs with
// only its shared secret in hand.
func (s *Server) adminOrAP(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.authorizedAdmin(r) && !s.authorized(r) {
			s.rejectAuth(w, r, "invalid or missing token")
			return
		}
		s.auth.reset(remoteIP(r))
		next(w, r)
	}
}

func (s *Server) authorizedAdmin(r *http.Request) bool {
	// Automation keeps using the Bearer token; the UI uses the session cookie.
	if s.AdminToken != "" && bearerMatches(r, s.AdminToken) {
		return true
	}
	_, ok := s.sessionFrom(r)
	return ok
}

// Failed-attempt budget. Five guesses per minute from one address is far more
// than an operator needs and far less than a token search wants; the lockout is
// short enough that mistyping a token a few times only costs a minute.
const (
	authFailureLimit  = 5
	authFailureWindow = time.Minute
	authLockoutWindow = time.Minute
)

// authTracker counts failed credential attempts per source address.
//
// It deliberately keys on the TCP source (r.RemoteAddr), not X-Forwarded-For:
// the latter is client-controlled, so rotating it would turn the budget into a
// no-op. Behind a reverse proxy every operator shares the proxy address, which
// is acceptable for a single-admin controller.
type authTracker struct {
	mu   sync.Mutex
	byIP map[string]*authWindow
}

type authWindow struct {
	first    time.Time
	failures int
	until    time.Time
}

// blocked reports how long an address must wait before trying again.
func (t *authTracker) blocked(ip string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.byIP[ip]
	if e == nil || e.until.IsZero() {
		return 0, false
	}
	left := time.Until(e.until)
	if left <= 0 {
		delete(t.byIP, ip)
		return 0, false
	}
	return left, true
}

// fail records one refused attempt and reports a new lockout when the budget is
// exhausted.
func (t *authTracker) fail(ip string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.byIP == nil {
		t.byIP = map[string]*authWindow{}
	}
	e := t.byIP[ip]
	if e == nil || now.Sub(e.first) > authFailureWindow {
		e = &authWindow{first: now}
		t.byIP[ip] = e
	}
	e.failures++
	if e.failures < authFailureLimit {
		return 0, false
	}
	e.failures = 0
	e.first = now
	e.until = now.Add(authLockoutWindow)
	return authLockoutWindow, true
}

// reset clears the budget after a credential was accepted.
func (t *authTracker) reset(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byIP, ip)
}

// rejectAuth answers an unauthenticated request.
//
// A missing credential is a configuration step, not a guess — the UI has to load
// before it can ask for a token — so it only gets 401. A credential that was
// presented but refused is a guess: it consumes the budget and, once exhausted,
// answers 429 with Retry-After. The credential check itself happens before this,
// so a correct token is never blocked by someone else's failures.
func (s *Server) rejectAuth(w http.ResponseWriter, r *http.Request, reason string) {
	ip := remoteIP(r)
	if bearerPresent(r) {
		if wait, ok := s.auth.blocked(ip); ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			writeError(w, http.StatusTooManyRequests,
				fmt.Errorf("too many failed authentication attempts; try again in %s", wait.Round(time.Second)))
			return
		}
		if wait, locked := s.auth.fail(ip); locked {
			s.Store.LogEvent(r.Context(), "auth", "", "authentication locked out for "+ip)
			if s.Log != nil {
				s.Log.Warn("authentication failures locked out", "ip", ip)
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			writeError(w, http.StatusTooManyRequests,
				fmt.Errorf("too many failed authentication attempts; try again in %s", wait.Round(time.Second)))
			return
		}
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="ac-controller"`)
	writeError(w, http.StatusUnauthorized, errors.New(reason))
}

// remoteIP strips the port from r.RemoteAddr, yielding the address the rate
// limit is keyed on.
func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func bearerPresent(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   version.Version,
		"commit":    version.Commit,
		"ac_id":     s.ACID,
		"devices":   n,
		"pubkey":    s.Key.PublicHex(),
		"algorithm": "ed25519",
		// The management surface always requires a login account now. Health
		// stays unauthenticated on purpose: it is what a monitor or an AP about
		// to enrol can reach.
		"admin_auth_required": true,
		"login_required":      true,
	})
}

// handlePubKey exposes the verification key so an AP can pin it during
// enrolment instead of trusting the package blindly.
func (s *Server) handlePubKey(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(s.Key.PublicHex() + "\n"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"algorithm":  "ed25519",
		"public_key": s.Key.PublicHex(),
		"ac_id":      s.ACID,
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.Store.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.annotatePresence(devices)
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}
	d, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	one := []store.Device{d}
	s.annotatePresence(one)
	writeJSON(w, http.StatusOK, one[0])
}

func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}

	var req struct {
		TemplateID string `json:"template_id"`
	}
	// An empty body is legitimate: fall back to the seeded default template.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		req.TemplateID = s.adoptTemplateFor(r, mac)
	}

	res, err := s.Provision.Adopt(r.Context(), mac, req.TemplateID)
	if err != nil {
		writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRepush(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}
	res, err := s.Provision.Repush(r.Context(), mac)
	if err != nil {
		writeProvisionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleSetDeviceNetwork stores the per-unit layer-2 values: which ports this AP
// wires, the per-VLAN port maps, and its management addresses.
//
// Kept separate from the template because these are properties of one box, not
// of the fleet. Sending an empty object clears the override, which returns the
// device to "template as written".
func (s *Server) handleSetDeviceNetwork(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}

	var ov model.DeviceNetwork
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ov); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
	}

	stored := ""
	if !ov.IsZero() {
		blob, err := json.Marshal(ov)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored = string(blob)
	}
	if err := s.Store.SetDeviceNetwork(r.Context(), mac, stored); err != nil {
		writeStoreError(w, err)
		return
	}

	// Return the stored form so the caller sees exactly what took effect.
	d, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleSetDeviceRadios stores the per-unit radio values: channel, mode, power
// or country this AP should use instead of the shared template. Sending an empty
// list clears the override and returns the unit to "template as written".
func (s *Server) handleSetDeviceRadios(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}

	var ov model.DeviceRadioOverrides
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ov); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
			return
		}
	}
	seen := map[int]bool{}
	for _, radio := range ov.Radios {
		if radio.Index < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("radio index %d is invalid", radio.Index))
			return
		}
		if seen[radio.Index] {
			writeError(w, http.StatusBadRequest, fmt.Errorf("radio index %d appears twice", radio.Index))
			return
		}
		seen[radio.Index] = true
	}

	stored := ""
	if !ov.IsZero() {
		blob, err := json.Marshal(ov)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored = string(blob)
	}
	if err := s.Store.SetDeviceRadios(r.Context(), mac, stored); err != nil {
		writeStoreError(w, err)
		return
	}

	// Return the stored form so the caller sees exactly what took effect.
	d, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}
	if err := s.Store.Release(r.Context(), mac); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "release", mac, "released to pending")
	d, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handlePackage returns the exact bytes the AP should download. ?rev=N selects a
// specific revision; omitting it serves the newest.
func (s *Server) handlePackage(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}
	rev := 0
	if raw := r.URL.Query().Get("rev"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, errors.New("rev must be a non-negative integer"))
			return
		}
		rev = n
	}

	pkg, blob, err := s.Provision.Package(r.Context(), mac, rev)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("acpkg-%s-rev%d.tar.gz", shortMAC(mac), pkg.Rev)))
	w.Header().Set("X-Ac-Rev", strconv.Itoa(pkg.Rev))
	w.Header().Set("X-Ac-Sha256", pkg.SHA256)
	w.Header().Set("ETag", `"`+pkg.SHA256+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	mac, ok := pathMAC(w, r)
	if !ok {
		return
	}
	list, err := s.Store.ListPackages(r.Context(), mac)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": list})
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	refs, err := s.Store.TemplateRefs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list, "refs": refs})
}

func (s *Server) handleTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Store.GetTemplate(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var t model.Template
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	if err := t.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.UpsertTemplate(r.Context(), t); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "template", "", "template "+t.ID+" saved")
	writeJSON(w, http.StatusCreated, t)
}

// handleDeleteTemplate refuses to delete a template that a device or group
// still points at. Cascading would leave those objects with a configuration
// source that no longer exists, and nothing would repair them until someone
// noticed the next package generation failing.
func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetTemplate(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	refs, err := s.Store.CountTemplateRefs(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if refs.Devices+refs.Groups > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf(
			"模板仍被引用（设备 %d、分组 %d），请先改绑后再删除", refs.Devices, refs.Groups))
		return
	}
	// The seeded default is the fallback for adoption; deleting the last
	// template would leave no valid CLI/API default.
	n, err := s.Store.CountTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if n <= 1 {
		writeError(w, http.StatusConflict, fmt.Errorf("至少保留一个模板"))
		return
	}
	if err := s.Store.DeleteTemplate(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	s.Store.LogEvent(r.Context(), "template", "", "template "+id+" deleted")
	w.WriteHeader(http.StatusNoContent)
}

// handleCloneTemplate copies a template under a new id. Cloning is the cheap
// way to try a different SSID set on one AP group without editing the template
// that production devices already run.
func (s *Server) handleCloneTemplate(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("id")
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	src, err := s.Store.GetTemplate(r.Context(), srcID)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	dst := src
	dst.ID = strings.TrimSpace(req.ID)
	if dst.ID == "" {
		for attempt := 0; attempt < 4; attempt++ {
			buf := make([]byte, 4)
			if _, err := rand.Read(buf); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			candidate := srcID + "-copy-" + hex.EncodeToString(buf)
			if _, err := s.Store.GetTemplate(r.Context(), candidate); errors.Is(err, store.ErrNotFound) {
				dst.ID = candidate
				break
			}
		}
		if dst.ID == "" {
			writeError(w, http.StatusInternalServerError, errors.New("could not allocate a clone id"))
			return
		}
	}
	if strings.ContainsAny(dst.ID, " \t\r\n/") {
		writeError(w, http.StatusBadRequest, errors.New("模板 ID 不能包含空白或斜杠"))
		return
	}
	if dst.ID == srcID {
		writeError(w, http.StatusBadRequest, errors.New("克隆模板必须使用新的 ID"))
		return
	}
	if _, err := s.Store.GetTemplate(r.Context(), dst.ID); err == nil {
		writeError(w, http.StatusConflict, fmt.Errorf("模板 %q 已存在", dst.ID))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	dst.Name = strings.TrimSpace(req.Name)
	if dst.Name == "" {
		dst.Name = src.Name + " 副本"
	}
	if err := dst.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.UpsertTemplate(r.Context(), dst); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "template", "", "template "+srcID+" cloned to "+dst.ID)
	writeJSON(w, http.StatusCreated, dst)
}

// handleTemplateSSIDs replaces only the SSID list of a stored template.
//
// The SSID page edits one shared template across a fleet; sending the whole
// template from there would couple every SSID save to the bridge/VLAN state the
// page never displays — a stale draft could silently revert network changes
// made through the ports page (and vice versa). SSIDs and the layer-2 fabric
// are separate concerns, so this endpoint merges the SSID list into whatever is
// currently stored and leaves everything else untouched.
//
// A wireless-only template (Bridge zeroed) keeps working: its SSIDs reference
// netifd interfaces that exist locally on the AP, so the network name is a
// label this endpoint never crosses-checks against bridge.interfaces when the
// template declares none.
func (s *Server) handleTemplateSSIDs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		SSIDs []model.SSID `json:"ssids"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	t, err := s.Store.GetTemplate(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	t.SSIDs = req.SSIDs
	if err := t.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.UpsertTemplate(r.Context(), t); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "template", "", "template "+t.ID+" ssids updated ("+
		strconv.Itoa(len(t.SSIDs))+" ssids)")
	writeJSON(w, http.StatusOK, t)
}

// handleTemplateRename changes a template's display name in place. The id is
// the stable key devices trust, so a rename must never touch it.
func (s *Server) handleTemplateRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	t, err := s.Store.GetTemplate(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	t.Name = strings.TrimSpace(req.Name)
	if err := t.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.UpsertTemplate(r.Context(), t); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "template", "", "template "+t.ID+" renamed to "+t.Name)
	writeJSON(w, http.StatusOK, t)
}

// pathMAC normalises the {mac} path value, answering 400 when it is malformed.
func pathMAC(w http.ResponseWriter, r *http.Request) (string, bool) {
	mac := discovery.NormalizeMAC(r.PathValue("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return "", false
	}
	return mac, true
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

func writeProvisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, provision.ErrTemplateNotFound),
		errors.Is(err, provision.ErrNotAdopted),
		errors.Is(err, provision.ErrNoTemplate):
		writeError(w, http.StatusBadRequest, err)
	default:
		// Template validation failures surface here as plain errors.
		writeError(w, http.StatusUnprocessableEntity, err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func shortMAC(mac string) string {
	return strings.ReplaceAll(mac, ":", "")
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
