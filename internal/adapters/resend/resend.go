// Package resend implements app.Mailer over Resend's HTTP API with the
// standard library: POST https://api.resend.com/emails.
package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"enquoir/internal/app"
)

var _ app.Mailer = (*Mailer)(nil)

const defaultBaseURL = "https://api.resend.com"

// userAgent is required: Resend refuses requests without one (403).
const userAgent = "enquoir/1.0"

// Mailer sends through Resend.
type Mailer struct {
	apiKey  string
	from    string
	client  *http.Client
	baseURL string
}

// New builds a Mailer sending as from ("Name <address>").
func New(apiKey, from string, client *http.Client) *Mailer {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Mailer{apiKey: apiKey, from: from, client: client, baseURL: defaultBaseURL}
}

type tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type request struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
	Text    string   `json:"text"`
	Tags    []tag    `json:"tags,omitempty"`
}

// Send posts one email, with its idempotency key, so a retry never
// sends it twice (Resend keeps keys for 24 hours).
func (m *Mailer) Send(ctx context.Context, e app.OutgoingEmail) (string, error) {
	body := request{From: m.from, To: []string{string(e.To)}, Subject: e.Subject, HTML: e.HTML, Text: e.Text}
	for k, v := range e.Tags {
		body.Tags = append(body.Tags, tag{Name: k, Value: v})
	}
	bs, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/emails", bytes.NewReader(bs))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if e.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", e.IdempotencyKey)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resend: sending: %v: %w", err, app.ErrAmbiguous)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode == http.StatusOK {
		var ok struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(respBody, &ok); err != nil || ok.ID == "" {
			return "", fmt.Errorf("resend: unreadable success response %s: %w", respBody, app.ErrAmbiguous)
		}
		return ok.ID, nil
	}

	var failure struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(respBody, &failure)
	detail := fmt.Sprintf("resend: status %d %s: %s", resp.StatusCode, failure.Name, failure.Message)
	switch {
	case resp.StatusCode == http.StatusConflict && failure.Name == "invalid_idempotent_request":
		// The key was used with a different payload: an earlier attempt
		// was sent, with a link minted then - which is still valid.
		return "", fmt.Errorf("%s: %w", detail, app.ErrAlreadySent)
	case resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusUnauthorized, resp.StatusCode >= 500:
		// Busy, rate limited, down, or a key not yet configured:
		// retrying with the same key may succeed.
		return "", fmt.Errorf("%s: %w", detail, app.ErrAmbiguous)
	default:
		// 400, 403, 422: Resend refused this message for good (invalid
		// recipient, unverified sending domain).
		return "", fmt.Errorf("%s: %w", detail, app.ErrRejected)
	}
}
