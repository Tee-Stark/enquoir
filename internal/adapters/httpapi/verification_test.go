package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"enquoir/internal/domain"
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

// Editing the spending policy and approving an escalated payment are
// both at least as sensitive as creating an agent or inviting a
// member - both need a verified email too, like every other
// credential-shaping or money-moving route. Rejecting needs no
// verification, matching disabling an integration: cutting a payment
// off is always allowed.
func TestVerification_UnverifiedCannotUpdatePolicyOrApproveEscalation(t *testing.T) {
	f := newAuthFixture(t, true)
	rec := f.do(t, http.MethodPost, "/v1/auth/signup", `{"email":"unverified-owner@example.com","password":"correct horse battery","organisation_name":"Org"}`)
	cookie := sessionCookie(t, rec)
	var body authBody
	json.Unmarshal(rec.Body.Bytes(), &body)
	org := domain.OrgID(body.Organisation.ID)
	f.payments.Add(payment("key_esc", org, domain.AWAITING_APPROVAL, ""))

	if rec := f.do(t, http.MethodPut, "/v1/policies", `{"currency_rules":[]}`, cookie); rec.Code != http.StatusForbidden {
		t.Errorf("unverified owner updating the policy: %d, want 403", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/approve", "", cookie); rec.Code != http.StatusForbidden {
		t.Errorf("unverified owner approving an escalation: %d, want 403", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/reject", "", cookie); rec.Code != http.StatusNoContent {
		t.Errorf("unverified owner rejecting an escalation: %d %s, want 204", rec.Code, rec.Body)
	}

	f.verify(t, "unverified-owner@example.com")
	if rec := f.do(t, http.MethodPut, "/v1/policies", `{"currency_rules":[]}`, cookie); rec.Code != http.StatusOK {
		t.Errorf("verified owner updating the policy: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/approve", "", cookie); rec.Code != http.StatusNoContent {
		t.Errorf("verified owner approving: %d %s, want 204", rec.Code, rec.Body)
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
