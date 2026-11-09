package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var testSeed = bytes.Repeat([]byte{7}, ed25519.SeedSize)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	return ed25519.NewKeyFromSeed(testSeed)
}

func testInput() Input {
	return Input{
		Rev:         1,
		DeviceMAC:   "8c:de:52:11:22:33",
		ACID:        "2dbbf8ca3915649f4fbc64f56a31f1e9",
		TemplateID:  "default",
		Models:      []string{"XR1710G"},
		GeneratedAt: time.Date(2026, 10, 31, 0, 30, 0, 0, time.UTC),
		Files: map[string][]byte{
			"wireless": []byte("config wifi-device 'ac_radio0'\n"),
			"network":  []byte("config interface 'ac_vlan10'\n"),
		},
	}
}

func TestBuildVerifyRoundTrip(t *testing.T) {
	priv := testKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	built, archive, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Signature == "" {
		t.Fatal("signature is empty")
	}
	if len(built.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(built.Files))
	}

	got, payload, err := Extract(archive, pub)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Rev != 1 || got.DeviceMAC != "8c:de:52:11:22:33" || got.TemplateID != "default" {
		t.Errorf("unexpected manifest: %+v", got)
	}
	if !got.GeneratedAt.Equal(built.GeneratedAt) {
		t.Errorf("generated_at = %v, want %v", got.GeneratedAt, built.GeneratedAt)
	}
	if !got.AllowsModel("xr1710g") {
		t.Error("model whitelist should match case-insensitively")
	}
	if got.AllowsModel("EAP670") {
		t.Error("model whitelist should reject other models")
	}
	if string(payload["wireless"]) != string(testInput().Files["wireless"]) {
		t.Errorf("wireless payload = %q", payload["wireless"])
	}
}

// Same inputs must produce byte-identical archives, otherwise every rebuild
// looks like a content change to the AP and defeats revision diffing.
func TestBuildIsDeterministic(t *testing.T) {
	priv := testKey(t)
	_, a, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build a: %v", err)
	}
	_, b, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two builds of identical input differ")
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	_, archive, err := Build(testInput(), testKey(t))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	otherPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	_, err = Verify(archive, otherPriv.Public().(ed25519.PublicKey))
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

func TestTamperedPayloadIsRejected(t *testing.T) {
	priv := testKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	m, _, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Re-archive with modified content but the original (stale) manifest.
	tampered := repack(t, m, map[string][]byte{
		"wireless": []byte("config wifi-iface 'evil'\n"),
		"network":  testInput().Files["network"],
	})
	_, err = Verify(tampered, pub)
	if err == nil {
		t.Fatal("tampered payload verified")
	}
	if errors.Is(err, ErrSignature) {
		t.Fatal("expected a hash mismatch, got a signature failure")
	}
}

func TestUnsignedExtraFileIsRejected(t *testing.T) {
	priv := testKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	m, _, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	smuggled := repack(t, m, map[string][]byte{
		"wireless": testInput().Files["wireless"],
		"network":  testInput().Files["network"],
		"apply.sh": []byte("#!/bin/sh\nreboot\n"),
	})

	if _, _, err := Extract(smuggled, pub); err == nil {
		t.Fatal("unsigned apply.sh was accepted")
	}
}

func TestMissingFileIsRejected(t *testing.T) {
	priv := testKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	m, _, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	incomplete := repack(t, m, map[string][]byte{
		"wireless": testInput().Files["wireless"],
	})

	if _, _, err := Extract(incomplete, pub); err == nil {
		t.Fatal("package missing a manifest-listed file was accepted")
	}
}

func TestBuildRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "a/b", ".", "..", ""} {
		in := testInput()
		in.Files = map[string][]byte{name: []byte("x")}
		if _, _, err := Build(in, testKey(t)); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}

func TestBuildRejectsEmptyInput(t *testing.T) {
	in := testInput()
	in.Files = nil
	if _, _, err := Build(in, testKey(t)); err == nil {
		t.Error("empty package was accepted")
	}
	in = testInput()
	in.Rev = 0
	if _, _, err := Build(in, testKey(t)); err == nil {
		t.Error("rev 0 was accepted")
	}
}

// repack rebuilds an archive from an existing manifest and arbitrary payload,
// simulating an attacker who cannot re-sign manifest.json.
func repack(t *testing.T, m Manifest, files map[string][]byte) []byte {
	t.Helper()

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  m.GeneratedAt,
		}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	write(ManifestName, manifestBytes)
	for name, body := range files {
		write(name, body)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestSignatureCoversManifestFields(t *testing.T) {
	priv := testKey(t)
	m, _, err := Build(testInput(), priv)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Flip the revision: the signature must no longer verify.
	m.Rev = 99
	msg, err := signingBytes(m)
	if err != nil {
		t.Fatalf("signing bytes: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if ed25519.Verify(priv.Public().(ed25519.PublicKey), msg, sig) {
		t.Error("signature still verifies after the revision changed")
	}
}
