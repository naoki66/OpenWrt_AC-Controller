package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/naoki66/ac-controller/shared/secret"
)

func (c *Client) endpoint(path string) (string, error) {
	u, err := url.ParseRequestURI(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return "", errors.New("controller supplied an invalid relative path")
	}
	return c.BaseURL + u.RequestURI(), nil
}

func (c *Client) authorize(ctx context.Context, req *http.Request, body []byte) (string, error) {
	if c.deviceKey == "" {
		if c.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret.AuthToken(c.Secret))
		}
		return "", nil
	}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/ap/challenge", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(probe)
	if err != nil {
		return "", fmt.Errorf("request device challenge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request device challenge: HTTP %d", resp.StatusCode)
	}
	const challengeHexLen = 2 * (8 + 24 + 32)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, challengeHexLen+1))
	if err != nil || len(raw) != challengeHexLen {
		return "", errors.New("invalid device challenge length")
	}
	challenge := string(raw)
	if _, err := hex.DecodeString(challenge); err != nil {
		return "", errors.New("invalid device challenge encoding")
	}
	req.Header.Set(secret.DeviceMACHeader, c.deviceMAC)
	req.Header.Set(secret.DeviceChallengeHeader, challenge)
	req.Header.Set(secret.DeviceProofHeader, secret.DeviceProof(c.deviceKey, c.deviceMAC, challenge, req.Method, req.URL.RequestURI(), body))
	return challenge, nil
}
