package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// downloadFixture serves body at /fw and lets a test truncate the response or
// change the status.
type downloadFixture struct {
	server *httptest.Server
}

func newDownloadFixture(t *testing.T, serve func(http.ResponseWriter, *http.Request)) *downloadFixture {
	t.Helper()
	f := &downloadFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(serve))
	t.Cleanup(f.server.Close)
	return f
}

// The image must land on disk intact and without leaving the staging file
// behind: /tmp is small on an AP, and a stale .part after a reboot is waste.
func TestDownloadFirmwareStreamsToFile(t *testing.T) {
	blob := []byte(strings.Repeat("firmware-bytes-", 1024))
	f := newDownloadFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	client := NewClient(f.server.URL, "")
	dest := filepath.Join(t.TempDir(), "fw.img")

	if err := client.DownloadFirmware(context.Background(), "/fw", int64(len(blob)), dest, sha256Hex(blob)); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if string(got) != string(blob) {
		t.Errorf("image = %d bytes, want %d", len(got), len(blob))
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Errorf("staging file left behind: %v", err)
	}
}

// A digest mismatch is a hard rejection: the file must not exist, so sysupgrade
// can never be pointed at it.
func TestDownloadFirmwareRejectsDigestMismatch(t *testing.T) {
	blob := []byte("image")
	f := newDownloadFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	client := NewClient(f.server.URL, "")
	dest := filepath.Join(t.TempDir(), "fw.img")

	err := client.DownloadFirmware(context.Background(), "/fw", int64(len(blob)), dest, sha256Hex([]byte("other")))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	for _, p := range []string{dest, dest + ".part"} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("rejected image left %s behind", p)
		}
	}
}

// A response shorter than the advertised size must be reported rather than
// renamed into place: a truncated sysupgrade image bricks the AP.
func TestDownloadFirmwareRejectsTruncatedBody(t *testing.T) {
	blob := []byte(strings.Repeat("x", 32))
	f := newDownloadFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob[:20])
	})
	client := NewClient(f.server.URL, "")
	dest := filepath.Join(t.TempDir(), "fw.img")

	err := client.DownloadFirmware(context.Background(), "/fw", int64(len(blob)), dest, "")
	if err == nil || !strings.Contains(err.Error(), "controller said") {
		t.Fatalf("err = %v, want a byte-count mismatch", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("truncated image was renamed into place")
	}
}

// A body larger than advertised must be refused rather than silently clipped.
func TestDownloadFirmwareRejectsOversizedBody(t *testing.T) {
	blob := []byte(strings.Repeat("x", 64))
	f := newDownloadFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	client := NewClient(f.server.URL, "")
	dest := filepath.Join(t.TempDir(), "fw.img")

	err := client.DownloadFirmware(context.Background(), "/fw", int64(len(blob)-1), dest, "")
	if err == nil || !strings.Contains(err.Error(), "controller said") {
		t.Fatalf("err = %v, want a byte-count mismatch", err)
	}
}

func TestDownloadFirmwareReportsHTTPStatus(t *testing.T) {
	f := newDownloadFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such build", http.StatusNotFound)
	})
	client := NewClient(f.server.URL, "")
	dest := filepath.Join(t.TempDir(), "fw.img")

	err := client.DownloadFirmware(context.Background(), "/fw", 10, dest, "")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the HTTP status", err)
	}
}
