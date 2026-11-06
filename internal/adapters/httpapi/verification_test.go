package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

func TestVerification_UnverifiedCanLookButNotIssueKeys(t *testing.T) {
	f := newAuthFixture(t, true)
	rec := f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody)
	cookie := sessionCookie(t, rec)

	if rec := f.do(t, http.MethodGet, "/v1/payments", "", cookie); rec.Code != http.StatusOK {
		t.Errorf("unverified reading payments: %d", rec.Code)
	}
	rec = f.do(t, http.MethodPost, "/v1/agents", `{"name":"bot"}`, cookie)
	if p := decodeProblem(t, rec); rec.Code != http.StatusForbidden || p.Code != "email_unverified" {
		t.Errorf("unverified creating an agent: %d %q, want 403 email_unverified", rec.Code, p.Code)
	}

	f.verify(t, "ada@example.com")
	if rec := f.do(t, http.MethodPost, "/v1/agents", `{"name":"bot"}`, cookie); rec.Code != http.StatusCreated {
		t.Errorf("verified creating an agent: %d %s", rec.Code, rec.Body)
	}
}

func TestVerification_Endpoints(t *testing.T) {
	f := newAuthFixture(t, true)
	cookie := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody))

	if rec := f.do(t, http.MethodPost, "/v1/auth/verify-email/resend", "", cookie); rec.Code != http.StatusAccepted {
		t.Errorf("resend: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.mailer.Sent()); n != 2 {
		t.Errorf("%d emails, want the sign-up one and the resent one", n)
	}

	bad := f.do(t, http.MethodPost, "/v1/auth/verify-email", `{"token":"tok_made_up"}`)
	if p := decodeProblem(t, bad); bad.Code != http.StatusBadRequest || p.Code != "token_invalid" {
		t.Errorf("a made-up token: %d %q, want 400 token_invalid", bad.Code, p.Code)
	}

	token := f.mailer.TokenTo("ada@example.com")
	if rec := f.do(t, http.MethodPost, "/v1/auth/verify-email", fmt.Sprintf(`{"token":%q}`, token)); rec.Code != http.StatusNoContent {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body)
	}
	rec := f.do(t, http.MethodPost, "/v1/auth/verify-email/resend", "", cookie)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "already_verified" {
		t.Errorf("resend once verified: %d %q, want 409 already_verified", rec.Code, p.Code)
	}
}
