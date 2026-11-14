package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// newTestListener starts a real listener on an ephemeral loopback port with a
// real UDP socket, so the tests exercise the actual wire handling.
func newTestListener(t *testing.T) *Listener {
	t.Helper()

	ctx := context.Background()

	l := &Listener{
		ACID:           "0123456789abcdef0123456789abcdef",
		HTTPPort:       8080,
		SecretRequired: true,
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

func TestBoundLookupOnlyReceivesOfferFromMatchingController(t *testing.T) {
	l := newTestListener(t)
	payload := []byte(`{"type":"AC_LOOKUP","ac_id":"` + l.ACID + `"}`)
	if offer, err := exchange(t, l, payload); err != nil || offer.ACID != l.ACID {
		t.Fatalf("lookup: %+v, %v", offer, err)
	}
}

func TestUnrelatedPacketsAreIgnored(t *testing.T) {
	l := newTestListener(t)

	for _, payload := range []string{
		`not json`,
		`{"type":"SOMETHING_ELSE","mac":"8c:de:52:11:22:55"}`,
		`{"type":"AP_DISCOVER","mac":"bogus"}`,
		`{"type":"AC_LOOKUP","ac_id":"another-controller"}`,
	} {
		if offer, err := exchange(t, l, []byte(payload)); err == nil {
			t.Errorf("payload %q should be ignored, got reply %+v", payload, offer)
		}
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
