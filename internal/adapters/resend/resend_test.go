package resend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enquoir/internal/app"
)

var msg = app.OutgoingEmail{
	To: "ada@example.com", Subject: "Confirm your email", HTML: "<p>hi</p>", Text: "hi",
	IdempotencyKey: "email/eml_1", Tags: map[string]string{"template": "verify_email"},
}

func TestSend_PostsTheDocumentedRequest(t *testing.T) {
	var got struct {
		From, Subject, HTML, Text string
		To                        []string
		Tags                      []struct{ Name, Value string }
	}
	var headers http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header
		if r.Method != http.MethodPost || r.URL.Path != "/emails" {
			t.Errorf("%s %s, want POST /emails", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	defer ts.Close()

	m := New("re_test_key", "Enquoir <no-reply@enquoir.etimi.me>", nil)
	m.baseURL = ts.URL
	id, err := m.Send(t.Context(), msg)
	if err != nil || id != "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if headers.Get("Authorization") != "Bearer re_test_key" || headers.Get("Idempotency-Key") != "email/eml_1" ||
		!strings.HasPrefix(headers.Get("User-Agent"), "enquoir/") || headers.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", headers)
	}
	if got.From != "Enquoir <no-reply@enquoir.etimi.me>" || len(got.To) != 1 || got.To[0] != "ada@example.com" ||
		got.Subject != msg.Subject || got.HTML != msg.HTML || got.Text != msg.Text {
		t.Errorf("body = %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0].Name != "template" || got.Tags[0].Value != "verify_email" {
		t.Errorf("tags = %+v", got.Tags)
	}
}

func TestSend_ClassifiesFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"already sent with an earlier payload", 409, `{"name":"invalid_idempotent_request","message":"used"}`, app.ErrAlreadySent},
		{"same key still in progress", 409, `{"name":"concurrent_idempotent_requests","message":"busy"}`, app.ErrAmbiguous},
		{"rate limited", 429, `{"name":"rate_limit_exceeded"}`, app.ErrAmbiguous},
		{"server error", 502, `bad gateway`, app.ErrAmbiguous},
		{"missing key", 401, `{"name":"missing_api_key"}`, app.ErrAmbiguous},
		{"invalid recipient", 422, `{"name":"validation_error","message":"invalid to"}`, app.ErrRejected},
		{"testing domain, not your address", 403, `{"name":"validation_error","message":"You can only send testing emails to your own email address"}`, app.ErrRejected},
		{"bad request", 400, `{"name":"validation_error"}`, app.ErrRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			m := New("re_test_key", "Enquoir <no-reply@enquoir.etimi.me>", nil)
			m.baseURL = ts.URL
			_, err := m.Send(t.Context(), msg)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "re_test_key") {
				t.Error("the API key leaked into an error")
			}
		})
	}
}

func TestSend_NetworkFailureIsAmbiguous(t *testing.T) {
	m := New("re_test_key", "x <a@b.co>", nil)
	m.baseURL = "http://127.0.0.1:1"
	if _, err := m.Send(t.Context(), msg); !errors.Is(err, app.ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous", err)
	}
}
