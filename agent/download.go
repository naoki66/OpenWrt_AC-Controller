package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// DownloadFirmware streams an image to dest while hashing it.
//
// The previous implementation buffered the whole image in RAM and then wrote a
// second copy. A sysupgrade image is tens of megabytes, and the agent shares a
// small AP's memory with hostapd: streaming to a file halves the peak and makes
// the bound enforceable without trusting the advertisement.
//
// size is the exact byte count published by the controller. It is both the read
// bound (size+1 lets an oversized response be recognised rather than truncated
// silently) and the truncation check. wantSHA256 is required by the controller
// contract; an empty digest is treated as "no verification requested" only
// because the field is optional in the wire format.
func (c *Client) DownloadFirmware(ctx context.Context, path string, size int64, dest, wantSHA256 string) error {
	// The shared client's 30s timeout is sized for a hello, not for tens of MB
	// over WiFi; a dedicated client keeps the download alive for its own budget
	// while still sharing the transport and its connection pool.
	ctx, cancel := context.WithTimeout(ctx, firmwareDownloadTimeout)
	defer cancel()

	targetURL, err := c.endpoint(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	if _, err := c.authorize(ctx, req, nil); err != nil {
		return err
	}

	download := &http.Client{Transport: c.HTTP.Transport, Timeout: firmwareDownloadTimeout, CheckRedirect: c.HTTP.CheckRedirect}
	resp, err := download.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	limit := int64(64 << 20)
	if size > 0 {
		limit = size + 1
	}
	reader := io.LimitReader(resp.Body, limit)

	target := dest + ".part"
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("stage image: %w", err)
	}

	digest := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, digest), reader)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return fmt.Errorf("download image: %w", copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(target)
		return fmt.Errorf("sync image: %w", syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return fmt.Errorf("close image: %w", closeErr)
	}
	if size > 0 && n != size {
		_ = os.Remove(target)
		return fmt.Errorf("image is %d bytes, controller said %d", n, size)
	}
	if wantSHA256 != "" {
		got := hex.EncodeToString(digest.Sum(nil))
		if !strings.EqualFold(got, wantSHA256) {
			_ = os.Remove(target)
			return errors.New("sha256 mismatch; image rejected")
		}
	}
	if err := os.Rename(target, dest); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("stage image: %w", err)
	}
	return nil
}
