// Package airwallex implements the Rail and Simulator ports against the
// real Airwallex sandbox API. Rail and Simulator are deliberately
// separate types sharing no state, each with its own transport and
// token cache, so nothing that only holds a Rail can reach a
// sandbox-only simulation call.
package airwallex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"enquoir/internal/app"
)

// SandboxBaseURL is the only base URL either constructor will accept.
// Real money must be impossible: there is no way to point this adapter
// anywhere else.
const SandboxBaseURL = "https://api.sandbox.airwallex.com"

const (
	tokenTTL           = 30 * time.Minute
	tokenSafetyMargin  = 1 * time.Minute
	maxRequestAttempts = 4
	initialBackoff     = 200 * time.Millisecond
)

// transport is the authenticated HTTP plumbing shared by Client and
// Simulator's implementations, but never shared as a single instance
// between them - each gets its own.
type transport struct {
	baseURL    string
	clientID   string
	apiKey     string
	httpClient *http.Client
	clock      app.Clock
	sleep      func(time.Duration)

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func newTransport(baseURL, clientID, apiKey string, httpClient *http.Client, clock app.Clock) *transport {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &transport{
		baseURL:    baseURL,
		clientID:   clientID,
		apiKey:     apiKey,
		httpClient: httpClient,
		clock:      clock,
		sleep:      time.Sleep,
	}
}

func requireSandbox(baseURL string) error {
	if baseURL != SandboxBaseURL {
		return fmt.Errorf("airwallex: refusing non-sandbox base URL %q: real money must be impossible", baseURL)
	}
	return nil
}

func (t *transport) ensureToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && t.clock.Now().Before(t.tokenExpiry.Add(-tokenSafetyMargin)) {
		return t.token, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/api/v1/authentication/login", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-client-id", t.clientID)
	req.Header.Set("x-api-key", t.apiKey)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("airwallex: login: %v: %w", err, app.ErrAmbiguous)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("airwallex: login: reading response: %v: %w", err, app.ErrAmbiguous)
	}
	// The sandbox answers a successful login with 201 Created; accept
	// any 2xx rather than one exact code.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("airwallex: login: status %d: %s: %w", resp.StatusCode, body, app.ErrAmbiguous)
	}

	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Token == "" {
		return "", fmt.Errorf("airwallex: login: unparseable response %s: %w", body, app.ErrAmbiguous)
	}

	t.token = parsed.Token
	t.tokenExpiry = t.clock.Now().Add(tokenTTL)
	return t.token, nil
}

// doJSON sends one authenticated JSON request, retrying with backoff on
// a 429. It never treats a non-2xx response as an error itself - that
// classification is the caller's job, since what counts as rejected
// versus ambiguous differs per endpoint.
func (t *transport) doJSON(ctx context.Context, method, path string, body any) (status int, respBody []byte, err error) {
	var bs []byte
	if body != nil {
		bs, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}

	backoff := initialBackoff
	for attempt := 1; attempt <= maxRequestAttempts; attempt++ {
		token, err := t.ensureToken(ctx)
		if err != nil {
			return 0, nil, err
		}

		var reader io.Reader
		if bs != nil {
			reader = bytes.NewReader(bs)
		}
		req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, reader)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := t.httpClient.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("airwallex: %s %s: %v: %w", method, path, err, app.ErrAmbiguous)
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, fmt.Errorf("airwallex: %s %s: reading response: %v: %w", method, path, readErr, app.ErrAmbiguous)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRequestAttempts {
			t.sleep(backoff)
			backoff *= 2
			continue
		}

		return resp.StatusCode, respBody, nil
	}

	return 0, nil, fmt.Errorf("airwallex: %s %s: exhausted retries on repeated rate limiting: %w", method, path, app.ErrAmbiguous)
}
