package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Field  string `json:"field"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding problem %s: %v", rec.Body, err)
	}
	return p
}

// Errors follow RFC 9457, with a stable machine-readable code a client
// can branch on without parsing the human-readable detail.
func TestErrorsAreProblemDetails(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path, body string
		status     int
		code       string
	}{
		{"invalid amount", http.MethodPost, "/v1/intents", `{"payee":"p","amount":"10.505","currency":"USD","reference":"R"}`, 400, "validation_failed"},
		{"malformed JSON", http.MethodPost, "/v1/intents", `{not json`, 400, "invalid_json"},
		{"unknown intent", http.MethodGet, "/v1/intents/nope", "", 404, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t, true)
			_, bearer := f.newAgent(t)
			var rec *httptest.ResponseRecorder
			if tc.method == http.MethodPost {
				rec = postIntentAs(t, f, bearer, tc.body)
			} else {
				rec = getIntentAs(t, f, bearer, "nope")
			}
			p := decodeProblem(t, rec)
			if rec.Code != tc.status || p.Status != tc.status {
				t.Errorf("status = %d (body %d), want %d", rec.Code, p.Status, tc.status)
			}
			if p.Code != tc.code {
				t.Errorf("code = %q, want %q", p.Code, tc.code)
			}
			if p.Title != http.StatusText(tc.status) || p.Type != "about:blank" {
				t.Errorf("type/title = %q/%q, want about:blank/%q", p.Type, p.Title, http.StatusText(tc.status))
			}
			if p.Detail == "" {
				t.Error("detail is empty")
			}
		})
	}
}
