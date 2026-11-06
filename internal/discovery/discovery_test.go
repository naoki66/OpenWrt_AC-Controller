package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// newTestListener starts a real listener on an ephemeral loopback port with a
// throwaway SQLite database, so the tests exercise the actual wire handling
// rather than a mock.
func newTestListener(t *testing.T) *Listener {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	l := &Listener{
		ACID:           "0123456789abcdef0123456789abcdef",
		HTTPPort:       8080,
		SecretRequired: true,
		Store:          st,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("bind udp: %v", err)
	}
	l.Addr = uc.LocalAddr().String()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.serve(runCtx, uc)
	}()

	t.Cleanup(func() {
		cancel()
		uc.Close()
		<-done
		st.Close()
	})
	return l
}

// exchange sends one packet to the listener and returns the decoded reply.
func exchange(t *testing.T, l *Listener, payload []byte) (proto.Offer, error) {
	t.Helper()

	conn, err := net.Dial("udp4", l.Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return proto.Offer{}, err
	}

	var offer proto.Offer
	if err := json.Unmarshal(buf[:n], &offer); err != nil {
		return proto.Offer{}, err
	}
	return offer, nil
}

func mustDiscover(t *testing.T, mac, hostname, firmware string) []byte {
	t.Helper()
	b, err := json.Marshal(proto.Discover{
		Type:     proto.MsgDiscover,
		V:        proto.ProtocolVersion,
		MAC:      mac,
		Model:    "XR1710G",
		Firmware: firmware,
		Hostname: hostname,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestOfferCarriesRegistrationEndpoint(t *testing.T) {
	l := newTestListener(t)

	offer, err := exchange(t, l, mustDiscover(t, "8C:DE:52:11:22:33", "ap250", "20261026"))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if offer.Type != proto.MsgOffer {
		t.Fatalf("type = %q, want %q", offer.Type, proto.MsgOffer)
	}
	if offer.V != proto.ProtocolVersion {
		t.Errorf("v = %d, want %d", offer.V, proto.ProtocolVersion)
	}
	if offer.ACID != l.ACID {
		t.Errorf("ac_id = %q, want %q", offer.ACID, l.ACID)
	}
	if offer.HTTPPort != l.HTTPPort {
		t.Errorf("http_port = %d, want %d", offer.HTTPPort, l.HTTPPort)
	}
	if !offer.SecretRequired {
		t.Error("secret_required = false, want true")
	}
	if offer.ACIP == "" {
		t.Error("ac_ip is empty")
	}
}

// A controller without AC_SHARED_SECRET must not tell the AP to send one.
func TestSecretNotRequiredWhenUnset(t *testing.T) {
	l := newTestListener(t)
	l.SecretRequired = false

	offer, err := exchange(t, l, mustDiscover(t, "8c:de:52:11:22:33", "ap250", "20261026"))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if offer.SecretRequired {
		t.Error("secret_required = true, want false")
	}
}

func TestDiscoveryRecordsPendingDevice(t *testing.T) {
	l := newTestListener(t)

	if _, err := exchange(t, l, mustDiscover(t, "8C:DE:52:11:22:33", "ap250", "20261026")); err != nil {
		t.Fatalf("discover: %v", err)
	}

	devices, err := l.Store.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("device count = %d, want 1", len(devices))
	}

	d := devices[0]
	if d.MAC != "8c:de:52:11:22:33" {
		t.Errorf("mac = %q, want lower-cased", d.MAC)
	}
	if d.State != store.StatePending {
		t.Errorf("state = %q, want %q", d.State, store.StatePending)
	}
	if d.Hostname != "ap250" || d.Model != "XR1710G" || d.Firmware != "20261026" {
		t.Errorf("unexpected fields: %+v", d)
	}
	if d.IP == "" {
		t.Error("ip is empty")
	}
}

func TestRepeatAnnounceIsIdempotent(t *testing.T) {
	l := newTestListener(t)
	ctx := context.Background()

	if _, err := exchange(t, l, mustDiscover(t, "8c:de:52:11:22:44", "ap251", "20261026")); err != nil {
		t.Fatalf("announce: %v", err)
	}
	first, err := l.Store.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if _, err := exchange(t, l, mustDiscover(t, "8c:de:52:11:22:44", "ap251", "20261101")); err != nil {
		t.Fatalf("re-announce: %v", err)
	}
	second, err := l.Store.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(second) != 1 {
		t.Fatalf("device count = %d, want 1 (announce must upsert)", len(second))
	}
	if second[0].Firmware != "20261101" {
		t.Errorf("firmware = %q, want updated value", second[0].Firmware)
	}
	if second[0].CreatedAt != first[0].CreatedAt {
		t.Error("created_at changed on re-announce")
	}
}

func TestUnrelatedPacketsAreIgnored(t *testing.T) {
	l := newTestListener(t)

	for _, payload := range []string{
		`not json`,
		`{"type":"SOMETHING_ELSE","mac":"8c:de:52:11:22:55"}`,
		`{"type":"AP_DISCOVER","mac":"bogus"}`,
	} {
		if offer, err := exchange(t, l, []byte(payload)); err == nil {
			t.Errorf("payload %q should be ignored, got reply %+v", payload, offer)
		}
	}

	n, err := l.Store.CountDevices(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("device count = %d, want 0", n)
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"8C:DE:52:11:22:33":     "8c:de:52:11:22:33",
		"8c-de-52-11-22-33":     "8c:de:52:11:22:33",
		"  8c:de:52:11:22:33  ": "8c:de:52:11:22:33",
		"":                      "",
		"bogus":                 "",
		"8c:de:52:11:22":        "",
		"8c:de:52:11:22:3g":     "",
	}
	for in, want := range cases {
		if got := NormalizeMAC(in); got != want {
			t.Errorf("NormalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
