package authplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	turnstileTimeout = 5 * time.Second
	// Anything larger than this is not a Turnstile response.
	turnstileMaxBody = 1 << 20
)

// turnstileClient never follows redirects: Go replays the request body on
// 307/308, which would hand the Turnstile secret to whatever the verifier
// redirects to (audit A-3). A redirect therefore fails the status check.
var turnstileClient = &http.Client{
	Timeout: turnstileTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// verifyToken runs the configured verifier and returns the hostname Cloudflare
// reports for the solved challenge.
func (cfg Config) verifyToken(ctx context.Context, token, remoteIP string) (string, error) {
	if cfg.verifyTurnstile != nil {
		return cfg.verifyTurnstile(ctx, cfg, token, remoteIP)
	}
	return verifyTurnstileHTTP(ctx, cfg, token, remoteIP)
}

func verifyTurnstileHTTP(ctx context.Context, cfg Config, token, remoteIP string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, turnstileTimeout)
	defer cancel()

	form := url.Values{}
	form.Set("secret", cfg.TurnstileSecret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	verifyURL := cfg.TurnstileVerifyURL
	if verifyURL == "" {
		verifyURL = defaultTurnstileVerifyURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, verifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := turnstileClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("turnstile: unexpected status %d", resp.StatusCode)
	}

	var result struct {
		Success    bool     `json:"success"`
		Hostname   string   `json:"hostname"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, turnstileMaxBody)).Decode(&result); err != nil {
		return "", fmt.Errorf("turnstile: decode response: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("turnstile: verification failed: %s", strings.Join(result.ErrorCodes, ","))
	}

	return result.Hostname, nil
}
