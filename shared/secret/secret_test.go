package secret

import "testing"

func TestAuthTokenDeterministicAndIsolated(t *testing.T) {
	a := AuthToken("hunter2")
	b := AuthToken("hunter2")
	if a != b {
		t.Fatalf("AuthToken not deterministic: %q != %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("AuthToken length = %d, want 64 hex chars", len(a))
	}
}

func TestPeerKeyDeterministicAndIsolated(t *testing.T) {
	a := PeerKey("hunter2")
	b := PeerKey("hunter2")
	if a != b {
		t.Fatalf("PeerKey not deterministic: %q != %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("PeerKey length = %d, want 64 hex chars", len(a))
	}
}

func TestAuthTokenDiffersFromPeerKey(t *testing.T) {
	if AuthToken("hunter2") == PeerKey("hunter2") {
		t.Fatal("AuthToken and PeerKey must differ for the same secret")
	}
}

func TestDistinctSecretsGiveDistinctKeys(t *testing.T) {
	if AuthToken("one") == AuthToken("two") {
		t.Fatal("different secrets must yield different auth tokens")
	}
	if PeerKey("one") == PeerKey("two") {
		t.Fatal("different secrets must yield different peer keys")
	}
}

func TestEmptySecret(t *testing.T) {
	// An empty secret still yields well-formed (though worthless) keys; callers
	// gate on the raw secret being non-empty before deriving.
	if AuthToken("") == PeerKey("") {
		t.Fatal("empty secret must still produce isolated domains")
	}
}
