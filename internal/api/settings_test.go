package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// helloAs posts a minimal valid hello carrying the given bearer secret, so
// tests can assert which credential the AP channel currently accepts.
func (f *fixture) helloAs(secret string) *http.Response {
	f.t.Helper()
	raw, err := json.Marshal(proto.Hello{MAC: testMAC, Model: "XR1710G"})
	if err != nil {
		f.t.Fatalf("marshal hello: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/v1/ap/hello", bytes.NewReader(raw))
	if err != nil {
		f.t.Fatalf("request: %v", err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatalf("hello: %v", err)
	}
	resp.Body.Close()
	return resp
}

// The devices page rotates the AP shared secret at runtime. These tests pin
// the contract: the override wins over the environment, an empty body restores
// the environment value, and the secret may never equal the admin token.

func TestSharedSecretRotateAndRevert(t *testing.T) {
	f := newFixture(t)

	// Rotation takes effect immediately on the AP channel.
	resp := f.do(http.MethodPut, "/api/v1/settings/shared-secret",
		map[string]string{"secret": "rotated-secret-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d", resp.StatusCode)
	}
	view := decode[map[string]any](t, resp)
	ss := view["shared_secret"].(map[string]any)
	if ss["effective"] != "rotated-secret-1" || ss["source"] != "settings" {
		t.Fatalf("view after rotate = %+v", ss)
	}

	// The AP channel now demands the new secret.
	if resp := f.helloAs("old-secret"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("hello with pre-rotation secret = %d, want 401", resp.StatusCode)
	}
	if resp := f.helloAs("rotated-secret-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("hello with rotated secret = %d, want 200", resp.StatusCode)
	}

	// ...and it survives in the store for the next restart.
	got, err := f.store.GetSetting(t.Context(), store.SettingSharedSecret)
	if err != nil || got != "rotated-secret-1" {
		t.Fatalf("stored setting = %q, %v", got, err)
	}

	// Empty secret clears the override: env default (empty in this fixture)
	// becomes effective again.
	resp = f.do(http.MethodPut, "/api/v1/settings/shared-secret", map[string]string{"secret": ""})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revert status = %d", resp.StatusCode)
	}
	view = decode[map[string]any](t, resp)
	ss = view["shared_secret"].(map[string]any)
	if ss["source"] != "env" || ss["effective"] != "" {
		t.Fatalf("view after revert = %+v", ss)
	}
	if _, err := f.store.GetSetting(t.Context(), store.SettingSharedSecret); err != store.ErrNotFound {
		t.Fatalf("setting still present after revert: %v", err)
	}
}

func TestSharedSecretValidation(t *testing.T) {
	f := newFixtureWithAuth(t, "admin-token-1234", "")
	do := func(secret string) *http.Response {
		return f.doAuth("admin-token-1234", http.MethodPut, "/api/v1/settings/shared-secret",
			map[string]string{"secret": secret})
	}

	// Too short.
	if resp := do("short"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short secret status = %d, want 400", resp.StatusCode)
	}

	// Equal to the admin token: APs knowing the shared secret could read the
	// management API, so this is rejected outright.
	if resp := do("admin-token-1234"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("secret == admin token status = %d, want 400", resp.StatusCode)
	}

	// The management API must never accept the AP shared secret instead of the
	// admin token — the two are independent credentials.
	resp := f.doAuth("admin-token-1234", http.MethodGet, "/api/v1/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings with admin token = %d, want 200", resp.StatusCode)
	}
	resp = f.do(http.MethodGet, "/api/v1/settings", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("settings without token = %d, want 401", resp.StatusCode)
	}
}

func TestNTPServersSettingAndHello(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodPut, "/api/v1/settings/ntp", map[string]any{
		"servers": []string{"10.0.10.1", "NTP.Aliyun.com", "bad host", "10.0.10.1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set NTP status = %d", resp.StatusCode)
	}
	view := decode[map[string]any](t, resp)
	list, _ := view["ntp"].([]any)
	if len(list) != 2 || list[0] != "10.0.10.1" || list[1] != "ntp.aliyun.com" {
		t.Fatalf("ntp view = %+v", view["ntp"])
	}

	// The next hello hands the list to an AP, including a pending one.
	_, reply := f.hello(t, proto.Hello{MAC: testMAC, Model: "XR1710G"})
	if len(reply.NTP) != 2 || reply.NTP[0] != "10.0.10.1" {
		t.Fatalf("hello NTP = %v", reply.NTP)
	}

	// It survives a controller restart and can be cleared from the page.
	raw, err := f.store.GetSetting(t.Context(), store.SettingNTPServers)
	if err != nil || !strings.Contains(raw, "ntp.aliyun.com") {
		t.Fatalf("stored NTP = %q, %v", raw, err)
	}
	resp = f.do(http.MethodPut, "/api/v1/settings/ntp", map[string]any{"servers": []string{}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear NTP status = %d", resp.StatusCode)
	}
	if _, err := f.store.GetSetting(t.Context(), store.SettingNTPServers); err != store.ErrNotFound {
		t.Fatalf("setting still present after clear: %v", err)
	}
}

func TestSecretOverrideBeatsEnv(t *testing.T) {
	// env secret set; the override must replace it for AP authentication.
	f := newFixtureWithAuth(t, "", "env-secret-12345678")
	if resp := f.login(defaultAdminUsername, defaultAdminPassword); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := f.do(http.MethodPut, "/api/v1/settings/shared-secret", map[string]string{"secret": "new-secret-987654"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d", resp.StatusCode)
	}

	// Old env secret is rejected on the AP channel; new one accepted.
	if resp := f.helloAs("env-secret-12345678"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("hello with env secret = %d, want 401", resp.StatusCode)
	}
	if resp := f.helloAs("new-secret-987654"); resp.StatusCode != http.StatusOK {
		t.Fatalf("hello with new secret = %d, want 200", resp.StatusCode)
	}
}
