package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

func throttled(t *testing.T) *authFixture {
	t.Helper()
	f := newAuthFixture(t, true)
	th := &app.Throttle{Counter: apptest.NewCounter(f.clock)}
	f.handler.Throttle = th
	f.handler.LogIn.Throttle = th
	return f
}

func fromIP(f *authFixture, path, body, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestSignUp_FivePerHourPerIP(t *testing.T) {
	f := throttled(t)
	for i := 0; i < 5; i++ {
		body := `{"email":"u` + strconv.Itoa(i) + `@example.com","password":"correct horse battery","organisation_name":"O"}`
		if rec := fromIP(f, "/v1/auth/signup", body, "203.0.113.7"); rec.Code != http.StatusCreated {
			t.Fatalf("sign-up %d: %d", i, rec.Code)
		}
	}
	rec := fromIP(f, "/v1/auth/signup", `{"email":"u9@example.com","password":"correct horse battery","organisation_name":"O"}`, "203.0.113.7")
	if p := decodeProblem(t, rec); rec.Code != http.StatusTooManyRequests || p.Code != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a sixth sign-up: %d %q Retry-After %q", rec.Code, p.Code, rec.Header().Get("Retry-After"))
	}
	if rec := fromIP(f, "/v1/auth/signup", `{"email":"v@example.com","password":"correct horse battery","organisation_name":"O"}`, "198.51.100.2"); rec.Code != http.StatusCreated {
		t.Errorf("another IP: %d", rec.Code)
	}
}

func TestLogIn_LockoutIs429WithRetryAfter(t *testing.T) {
	f := throttled(t)
	f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody)
	var rec *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		f.clock.Advance(13e9)
		rec = fromIP(f, "/v1/auth/login", `{"email":"ada@example.com","password":"wrong wrong wrong"}`, "203.0.113.9")
	}
	p := decodeProblem(t, rec)
	retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusTooManyRequests || p.Code != "account_locked" || retry < 3500 {
		t.Errorf("the tenth failure: %d %q Retry-After %d, want 429 account_locked for an hour", rec.Code, p.Code, retry)
	}
}

func TestClientIP_FromAConfiguredHeaderOnly(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")
	if got := clientIP(r, ""); got != "10.0.0.1" {
		t.Errorf("no header configured: %q, want the connection's address", got)
	}
	if got := clientIP(r, "X-Forwarded-For"); got != "203.0.113.1" {
		t.Errorf("X-Forwarded-For configured: %q, want the client", got)
	}
}
