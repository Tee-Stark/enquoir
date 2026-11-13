package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func payment(key string, org domain.OrgID, state domain.State, agent domain.AgentID) app.Payment {
	return app.Payment{Key: domain.IntentKey(key), OrgID: org, State: string(state), AgentID: agent}
}

type policyVersionBody struct {
	Version       int    `json:"version"`
	CurrencyRules []any  `json:"currency_rules"`
	CreatedBy     string `json:"created_by"`
}

// Editing the policy and running its dry-run need PermManagePolicy
// (owners and admins); any member may read the current version.
func TestPolicies_RolesAreEnforced(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)

	for _, role := range []domain.Role{domain.Viewer, domain.Operator} {
		member := f.memberAs(t, org, role)
		if rec := f.do(t, http.MethodGet, "/v1/policies/current", "", member); rec.Code != http.StatusOK {
			t.Errorf("%s GET current policy: %d, want 200", role, rec.Code)
		}
		if rec := f.do(t, http.MethodPut, "/v1/policies", `{"currency_rules":[]}`, member); rec.Code != http.StatusForbidden {
			t.Errorf("%s PUT policy: %d, want 403", role, rec.Code)
		}
		dryRunBody := `{"context":"treasury","currency":"USD","amount_minor":1000}`
		if rec := f.do(t, http.MethodPost, "/v1/policies/dry-run", dryRunBody, member); rec.Code != http.StatusForbidden {
			t.Errorf("%s POST dry-run: %d, want 403", role, rec.Code)
		}
	}

	// Owners may save a new version and run dry-run; the fake store
	// behind this fixture returns a fixed decision rather than really
	// evaluating a saved version against it - that round trip is
	// covered for real against Postgres in
	// internal/adapters/postgres/policies_integration_test.go. This
	// only proves the HTTP wiring: permissions, request parsing, and
	// the response shape.
	body := `{"currency_rules":[{"currency":"USD","max_transaction_minor":5000,"max_24h_minor":0,"max_7d_minor":0,"max_30d_minor":0,"max_per_hour_count":0,"approval_threshold_minor":0}]}`
	rec := f.do(t, http.MethodPut, "/v1/policies", body, owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT policy: %d %s", rec.Code, rec.Body)
	}
	var saved policyVersionBody
	json.Unmarshal(rec.Body.Bytes(), &saved)
	if saved.Version != 1 || saved.CreatedBy == "" {
		t.Errorf("saved = %+v, want version 1 attributed to the owner", saved)
	}

	dry := f.do(t, http.MethodPost, "/v1/policies/dry-run", `{"context":"treasury","currency":"USD","amount_minor":1000}`, owner)
	var decision struct {
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(dry.Body.Bytes(), &decision)
	if dry.Code != http.StatusOK || decision.Outcome != "allow" {
		t.Errorf("owner dry-run = %d %s, want 200 allow", dry.Code, dry.Body)
	}
}

// Approving/rejecting an escalated payment needs PermApproveEscalation
// (owners and admins); other roles are forbidden.
func TestEscalation_RolesAreEnforced(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	f.payments.Add(payment("key_esc", org, domain.AWAITING_APPROVAL, ""))

	for _, role := range []domain.Role{domain.Viewer, domain.Operator} {
		member := f.memberAs(t, org, role)
		if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/approve", "", member); rec.Code != http.StatusForbidden {
			t.Errorf("%s approve: %d, want 403", role, rec.Code)
		}
		if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/reject", "", member); rec.Code != http.StatusForbidden {
			t.Errorf("%s reject: %d, want 403", role, rec.Code)
		}
	}

	if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/approve", "", owner); rec.Code != http.StatusNoContent {
		t.Fatalf("owner approve: %d %s", rec.Code, rec.Body)
	}
	calls := f.signaler.Calls()
	if len(calls) != 1 || !calls[0].Approved || calls[0].Key != "key_esc" {
		t.Errorf("signaler calls = %+v, want one approval for key_esc", calls)
	}
}

func TestEscalation_RefusesAPaymentNotAwaitingApproval(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	f.payments.Add(payment("key_live", org, domain.AUTHORIZED, ""))

	rec := f.do(t, http.MethodPost, "/v1/payments/key_live/approve", "", owner)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "not_awaiting_approval" {
		t.Errorf("approving a non-escalated payment: %d %q, want 409 not_awaiting_approval", rec.Code, p.Code)
	}
}

// The agent's creator cannot approve their own agent's payment while
// another owner/admin exists.
func TestEscalation_CreatorExcludedFromApprovingOwnAgentsPayment(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	issued := f.createAgent(t, owner, "bot")
	f.payments.Add(payment("key_esc", org, domain.AWAITING_APPROVAL, domain.AgentID(issued.Agent.ID)))
	other := f.memberAs(t, org, domain.Admin)

	rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/approve", "", owner)
	if p := decodeProblem(t, rec); rec.Code != http.StatusForbidden || p.Code != "approver_excluded" {
		t.Errorf("creator approving their own agent's payment with another admin available: %d %q, want 403 approver_excluded", rec.Code, p.Code)
	}

	if rec := f.do(t, http.MethodPost, "/v1/payments/key_esc/approve", "", other); rec.Code != http.StatusNoContent {
		t.Errorf("the other admin approving: %d %s", rec.Code, rec.Body)
	}
}

// A malformed edit - an unsupported currency, or a negative limit -
// is rejected as 400, not silently saved with the bad currency
// dropped or the negative limit accepted as a ceiling nothing can
// pass.
func TestPolicies_UpdateRejectsAMalformedEdit(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner := f.newOrg(t)

	tests := map[string]string{
		"unsupported currency": `{"currency_rules":[{"currency":"JPY","max_transaction_minor":1000}]}`,
		"negative limit":       `{"currency_rules":[{"currency":"USD","max_transaction_minor":-1}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			rec := f.do(t, http.MethodPut, "/v1/policies", body, owner)
			if p := decodeProblem(t, rec); rec.Code != http.StatusBadRequest || p.Code != "validation_failed" {
				t.Errorf("PUT policy with a %s: %d %q, want 400 validation_failed", name, rec.Code, p.Code)
			}
		})
	}
}

func TestPolicies_DryRunRejectsAnUnknownContext(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner := f.newOrg(t)

	rec := f.do(t, http.MethodPost, "/v1/policies/dry-run", `{"context":"comerce","currency":"USD","amount_minor":1000}`, owner)
	if p := decodeProblem(t, rec); rec.Code != http.StatusBadRequest || p.Code != "validation_failed" {
		t.Errorf("dry-run with a mistyped context: %d %q, want 400 validation_failed", rec.Code, p.Code)
	}
}
