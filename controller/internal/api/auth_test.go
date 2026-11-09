package api

import (
	"net/http"
	"testing"

	secretkdf "github.com/naoki66/ac-controller/shared/secret"
)

// The deployment story this supports: a fresh controller is usable as soon as
// the page loads — log in as admin/123456 and change the password afterwards.
func TestDefaultAccountIsSeededAndLogsIn(t *testing.T) {
	f := newFixtureWithAuth(t, "", "")

	login := f.login(defaultAdminUsername, defaultAdminPassword)
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login with the seeded account = %d, want 200", login.StatusCode)
	}
	body := decode[struct {
		Username        string `json:"username"`
		DefaultPassword bool   `json:"default_password"`
	}](t, login)
	if body.Username != defaultAdminUsername {
		t.Errorf("login username = %q, want %q", body.Username, defaultAdminUsername)
	}
	if !body.DefaultPassword {
		t.Error("login did not report the seeded default password")
	}

	session := decode[struct {
		Authenticated   bool   `json:"authenticated"`
		Username        string `json:"username"`
		DefaultPassword bool   `json:"default_password"`
	}](t, f.do("GET", "/api/v1/auth/session", nil))
	if !session.Authenticated || session.Username != defaultAdminUsername {
		t.Errorf("session = %+v, want an authenticated admin", session)
	}
	if !session.DefaultPassword {
		t.Error("session did not report the seeded default password")
	}

	// The cookie is now enough to read the management surface.
	resp := f.do("GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /devices with the session cookie = %d, want 200", resp.StatusCode)
	}
}

// Management data (templates carry PSKs) must never be readable without a
// credential — unlike the old token-only scheme, "no token configured" is no
// longer open.
func TestManagementAPIRequiresLogin(t *testing.T) {
	f := newFixtureWithAuth(t, "", "")

	cases := []struct{ method, path string }{
		{"GET", "/api/v1/devices"},
		{"GET", "/api/v1/templates"},
		{"GET", "/api/v1/templates/default"},
		{"GET", "/api/v1/clients"},
		{"POST", "/api/v1/devices/" + testMAC + "/repush"},
	}
	for _, c := range cases {
		resp := f.do(c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a login = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}

	if resp := f.login(defaultAdminUsername, defaultAdminPassword); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	for _, c := range cases {
		resp := f.do(c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s %s with the session cookie = 401, want it to pass auth", c.method, c.path)
		}
	}
}

// A wrong password must be refused, and the guesses must be budgeted: five
// refusals from one address trip a short lockout, and a correct password still
// gets in so a shared address is not held hostage.
func TestWrongPasswordIsRefusedAndThrottled(t *testing.T) {
	f := newFixtureWithAuth(t, "", "")

	for i := 1; i < authFailureLimit; i++ {
		resp := f.login(defaultAdminUsername, "wrong-password")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong password attempt %d = %d, want 401", i, resp.StatusCode)
		}
	}

	resp := f.login(defaultAdminUsername, "wrong-password")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d, want 429", authFailureLimit, resp.StatusCode)
	}
	if v := resp.Header.Get("Retry-After"); v == "" || v == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", v)
	}

	// Further guesses are refused without being checked.
	resp = f.login(defaultAdminUsername, "wrong-password")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("attempt during lockout = %d, want 429", resp.StatusCode)
	}

	// A correct password during the lockout still works and clears the budget.
	resp = f.login(defaultAdminUsername, defaultAdminPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct login during lockout = %d, want 200", resp.StatusCode)
	}

	resp = f.login(defaultAdminUsername, "wrong-password")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password after a success = %d, want 401", resp.StatusCode)
	}
}

// The password change re-hashes, drops every other session, and keeps the
// caller logged in with a fresh cookie.
func TestChangePasswordRotatesCredentialsAndSessions(t *testing.T) {
	f := newFixtureWithAuth(t, "", "")
	if resp := f.login(defaultAdminUsername, defaultAdminPassword); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	oldCookie := f.cookie

	// The current password is required, so a session alone cannot change it.
	resp := f.do("PUT", "/api/v1/auth/account",
		map[string]string{"username": defaultAdminUsername, "current_password": "not-it", "new_password": "newpass123"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("change with a wrong current password = %d, want 401", resp.StatusCode)
	}

	resp = f.do("PUT", "/api/v1/auth/account",
		map[string]string{"username": defaultAdminUsername, "current_password": defaultAdminPassword, "new_password": "abc"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("change to a too-short password = %d, want 400", resp.StatusCode)
	}

	// Renaming to the same name with no new password is a no-op, not an error
	// the UI can act on silently.
	resp = f.do("PUT", "/api/v1/auth/account",
		map[string]string{"username": defaultAdminUsername, "current_password": defaultAdminPassword})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no-op change = %d, want 400", resp.StatusCode)
	}

	resp = f.do("PUT", "/api/v1/auth/account",
		map[string]string{"username": defaultAdminUsername, "current_password": defaultAdminPassword, "new_password": "newpass123"})
	body := decode[struct {
		Username        string `json:"username"`
		DefaultPassword bool   `json:"default_password"`
	}](t, resp)
	if body.Username != defaultAdminUsername || body.DefaultPassword {
		t.Errorf("change response = %+v, want admin with a non-default password", body)
	}

	// The old session cookie was revoked by the change.
	f.cookie = oldCookie
	resp = f.do("GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old session after a password change = %d, want 401", resp.StatusCode)
	}

	// The old password no longer works, the new one does.
	login := f.login(defaultAdminUsername, defaultAdminPassword)
	login.Body.Close()
	if login.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the old password = %d, want 401", login.StatusCode)
	}
	login = f.login(defaultAdminUsername, "newpass123")
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login with the new password = %d, want 200", login.StatusCode)
	}
}

// The login name is editable too: renaming drops the old session and makes the
// old name unusable while the password hash stays untouched.
func TestRenameAccountChangesLoginName(t *testing.T) {
	f := newFixtureWithAuth(t, "", "")
	if resp := f.login(defaultAdminUsername, defaultAdminPassword); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	oldCookie := f.cookie

	// A name with whitespace is refused.
	resp := f.do("PUT", "/api/v1/auth/account",
		map[string]string{"username": "ops admin", "current_password": defaultAdminPassword})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rename to a name with a space = %d, want 400", resp.StatusCode)
	}

	resp = f.do("PUT", "/api/v1/auth/account",
		map[string]string{"username": "ops", "current_password": defaultAdminPassword})
	body := decode[struct {
		Username        string `json:"username"`
		DefaultPassword bool   `json:"default_password"`
	}](t, resp)
	if body.Username != "ops" {
		t.Errorf("rename response username = %q, want ops", body.Username)
	}
	if !body.DefaultPassword {
		t.Error("renaming must not change the password, which is still the seeded default")
	}

	// The session issued under the old name is gone.
	f.cookie = oldCookie
	resp = f.do("GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old session after a rename = %d, want 401", resp.StatusCode)
	}

	// The old name no longer logs in; the new one does with the same password.
	login := f.login(defaultAdminUsername, defaultAdminPassword)
	login.Body.Close()
	if login.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the old name = %d, want 401", login.StatusCode)
	}
	login = f.login("ops", defaultAdminPassword)
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login with the new name = %d, want 200", login.StatusCode)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	f := newFixture(t)

	resp := f.do("POST", "/api/v1/auth/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("logout = %d, want 204", resp.StatusCode)
	}

	// The fixture still replays the cookie; the server must have dropped it.
	resp = f.do("GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /devices after logout = %d, want 401", resp.StatusCode)
	}
}

// The reason this exists: templates carry PSKs in clear text.
func TestManagementAPIRequiresAdminToken(t *testing.T) {
	const token = "s3cr3t-admin-token"
	f := newFixtureWithAuth(t, token, "")

	cases := []struct{ method, path string }{
		{"GET", "/api/v1/devices"},
		{"GET", "/api/v1/templates"},
		{"GET", "/api/v1/templates/default"},
		{"GET", "/api/v1/clients"},
		{"POST", "/api/v1/devices/" + testMAC + "/repush"},
	}
	for _, c := range cases {
		resp := f.do(c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", c.method, c.path, resp.StatusCode)
		}

		resp = f.doAuth(token, c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s %s with the admin token = 401, want it to pass auth", c.method, c.path)
		}
	}

	// A wrong token must be refused, not merely "a token".
	resp := f.doAuth("not-the-token", "GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /devices with a wrong token = %d, want 401", resp.StatusCode)
	}
}

// A guessing client must not be able to search the token space at network speed.
// The budget keys on the TCP source and only a *presented* credential consumes
// it: the UI asking for data before a token is configured stays a plain 401.
func TestBadTokenAttemptsAreThrottled(t *testing.T) {
	const token = "0123456789abcdef"
	f := newFixtureWithAuth(t, token, "")

	// No credential at all is a configuration step, not a guess.
	for i := 0; i < authFailureLimit+5; i++ {
		resp := f.do("GET", "/api/v1/devices", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("credential-less attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}

	// Attempts below the limit are ordinary refusals.
	for i := 1; i < authFailureLimit; i++ {
		resp := f.doAuth("wrong-token", "GET", "/api/v1/devices", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i, resp.StatusCode)
		}
	}

	// The limit trips the lockout and tells the caller how long to wait.
	resp := f.doAuth("wrong-token", "GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d, want 429", authFailureLimit, resp.StatusCode)
	}
	if v := resp.Header.Get("Retry-After"); v == "" || v == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", v)
	}

	// Further guesses are refused without being checked.
	resp = f.doAuth("wrong-token", "GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("attempt during lockout = %d, want 429", resp.StatusCode)
	}

	// A correct credential is checked before the budget, so an operator sharing
	// an address with a guesser is not locked out.
	resp = f.doAuth(token, "GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct token during lockout = %d, want 200", resp.StatusCode)
	}

	// ... and a successful authentication clears the budget.
	resp = f.doAuth("wrong-token", "GET", "/api/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token after a success = %d, want 401", resp.StatusCode)
	}
}

// The package download is the one endpoint both sides use: the AP has only the
// shared secret, an operator has a session or the admin token.
func TestPackageAcceptsEitherCredential(t *testing.T) {
	const admin, secret = "admin-token", "ap-secret"
	f := newFixtureWithAuth(t, admin, secret)
	path := "/api/v1/devices/" + testMAC + "/package"

	resp := f.do("GET", path, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("package without any credential = %d, want 401", resp.StatusCode)
	}

	// Neither credential may be accepted as the other: an AP holding only the
	// shared secret must still not be able to read the management surface.
	if resp := f.doAuth(secret, "GET", "/api/v1/templates", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("templates with the AP shared secret = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	for name, tok := range map[string]string{"admin": admin, "shared secret": secretkdf.AuthToken(secret)} {
		resp := f.doAuth(tok, "GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("package with the %s = 401, want it to pass auth", name)
		}
	}

	// A browser holding the session cookie can also download the bundle.
	if resp := f.login(defaultAdminUsername, defaultAdminPassword); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp = f.do("GET", path, nil)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("package with the session cookie = 401, want it to pass auth")
	}
}

// Management responses carry PSKs and topology; a reverse proxy or shared
// browser cache must not be allowed to keep a copy.
func TestManagementResponsesAreNotCacheable(t *testing.T) {
	const token = "0123456789abcdef"
	f := newFixtureWithAuth(t, token, "")

	for _, path := range []string{"/api/v1/templates", "/api/v1/devices", "/api/v1/firmwares"} {
		resp := f.doAuth(token, "GET", path, nil)
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, got)
		}
	}
}

// Health, the public key and the login surface are what an AP or a monitor can
// reach before it has any credential at all.
func TestHealthAndLoginSurfaceStayPublic(t *testing.T) {
	f := newFixtureWithAuth(t, "admin-token", "ap-secret")

	for _, path := range []string{"/api/v1/health", "/api/v1/ac/pubkey"} {
		resp := f.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 unauthenticated", path, resp.StatusCode)
		}
	}

	// The session probe answers rather than 401s: it is how the UI decides to
	// show the login form.
	probe := f.do("GET", "/api/v1/auth/session", nil)
	probe.Body.Close()
	if probe.StatusCode != http.StatusOK {
		t.Errorf("GET /auth/session = %d, want 200 unauthenticated", probe.StatusCode)
	}

	// The UI itself must load, otherwise there is nowhere to log in.
	resp := f.do("GET", "/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200 unauthenticated", resp.StatusCode)
	}
}
