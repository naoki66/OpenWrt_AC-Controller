package api

import (
	"net/http"
	"testing"
)

// Unconfigured means open, deliberately: a controller brought up on a
// management VLAN must not lock its operator out on the first boot. The
// trade-off is logged at startup and reported by /api/v1/health.
func TestManagementAPIStaysOpenWithoutAToken(t *testing.T) {
	f := newFixture(t)

	resp := f.do("GET", "/api/v1/devices", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /devices = %d, want 200 when no admin token is set", resp.StatusCode)
	}
	resp.Body.Close()

	health := f.do("GET", "/api/v1/health", nil)
	defer health.Body.Close()
	body := decode[struct {
		AdminAuthRequired bool `json:"admin_auth_required"`
	}](t, health)
	if body.AdminAuthRequired {
		t.Error("health reported admin_auth_required with no token configured")
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
// shared secret, an operator has only the admin token.
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

	for name, tok := range map[string]string{"admin": admin, "shared secret": secret} {
		resp := f.doAuth(tok, "GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("package with the %s = 401, want it to pass auth", name)
		}
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

// Health and the public key are what an AP or a monitor can reach before it has
// any credential at all.
func TestHealthAndPubKeyStayPublic(t *testing.T) {
	f := newFixtureWithAuth(t, "admin-token", "ap-secret")

	for _, path := range []string{"/api/v1/health", "/api/v1/ac/pubkey"} {
		resp := f.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 unauthenticated", path, resp.StatusCode)
		}
	}

	// The UI itself must load, otherwise there is nowhere to enter a token.
	resp := f.do("GET", "/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200 unauthenticated", resp.StatusCode)
	}
}
