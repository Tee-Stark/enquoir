package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func corsRouter(t *testing.T) http.Handler {
	t.Helper()
	f := newAuthFixture(t, true)
	f.handler.Origins = AllowedOrigins([]string{"https://app.enquoir.dev", "http://localhost:3000"})
	return NewRouter(f.handler)
}

func TestCORS_AllowsListedOriginsWithCredentials(t *testing.T) {
	r := corsRouter(t)

	req := httptest.NewRequest(http.MethodOptions, "/v1/payments", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d, want 204", rec.Code)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != "http://localhost:3000" || h.Get("Access-Control-Allow-Credentials") != "true" ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "DELETE") || !strings.Contains(strings.ToLower(h.Get("Access-Control-Allow-Headers")), "content-type") {
		t.Errorf("preflight headers = %v", h)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://app.enquoir.dev")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.enquoir.dev" || !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("simple request headers = %v", rec.Header())
	}
}

func TestCORS_IgnoresUnlistedOrigins(t *testing.T) {
	r := corsRouter(t)
	req := httptest.NewRequest(http.MethodOptions, "/v1/payments", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("an unlisted origin was allowed: %v", rec.Header())
	}
}

// A state-changing request a browser sends from an unlisted origin is
// refused before it reaches a handler, whatever cookie it carries.
// Requests with no Origin (agents, curl) and from the API's own host
// are unaffected.
func TestOriginCheck_RefusesCrossSiteWrites(t *testing.T) {
	r := corsRouter(t)
	send := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/signup", strings.NewReader(signUpBody))
		req.Host = "api.enquoir.dev"
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := send("https://evil.example")
	if p := decodeProblem(t, rec); rec.Code != http.StatusForbidden || p.Code != "forbidden_origin" {
		t.Errorf("cross-site POST: %d %q, want 403 forbidden_origin", rec.Code, p.Code)
	}
	for _, origin := range []string{"", "https://app.enquoir.dev", "https://api.enquoir.dev"} {
		if rec := send(origin); rec.Code == http.StatusForbidden {
			t.Errorf("POST with Origin %q refused", origin)
		}
	}
}

func TestPlaceholders_Return501(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/onboarding", ""},
		{http.MethodPut, "/v1/onboarding", `{}`},
	} {
		rec := f.do(t, r.method, r.path, r.body, cookie)
		if p := decodeProblem(t, rec); rec.Code != http.StatusNotImplemented || p.Code != "not_implemented" {
			t.Errorf("%s %s: %d %q, want 501 not_implemented", r.method, r.path, rec.Code, p.Code)
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/onboarding", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("onboarding without a session: %d, want 401", rec.Code)
	}
}
