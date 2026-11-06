package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body over the limit is refused with 413 before anything decodes
// it into memory, on unauthenticated routes as much as on the rest.
func TestBodiesOverTheLimitAreRefused(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)
	huge := `{"email":"a@example.com","password":"` + strings.Repeat("x", MaxRequestBodyBytes) + `","organisation_name":"A"}`

	for _, path := range []string{"/v1/auth/signup", "/v1/auth/login", "/v1/intents", "/v1/beneficiaries"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(huge))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if p := decodeProblem(t, rec); rec.Code != http.StatusRequestEntityTooLarge || p.Code != "body_too_large" {
			t.Errorf("POST %s with a %d-byte body: %d %q, want 413 body_too_large", path, len(huge), rec.Code, p.Code)
		}
	}
}

func TestBodiesUnderTheLimitStillWork(t *testing.T) {
	f := newAuthFixture(t, true)
	if rec := f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody); rec.Code != http.StatusCreated {
		t.Errorf("a normal sign-up: %d %s", rec.Code, rec.Body)
	}
}
