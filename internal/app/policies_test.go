package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

func TestGetCurrentPolicy_ReadsFromTheStore(t *testing.T) {
	policies := &apptest.FixedPolicy{}
	use := &app.GetCurrentPolicy{Policies: policies}

	v, err := use.Run(context.Background(), testOrg)
	if err != nil || v.OrgID != testOrg {
		t.Errorf("GetCurrentPolicy = %+v, %v, want the org's version", v, err)
	}
}

func TestUpdatePolicy_SavesANewVersionAndAudits(t *testing.T) {
	policies := &apptest.FixedPolicy{}
	auditLog := apptest.NewAuditLog()
	clock := apptest.NewClock()
	use := &app.UpdatePolicy{Policies: policies, Clock: clock, Audit: &app.Audit{Log: auditLog, IDs: &apptest.IDs{}, Clock: clock}}

	ctx := app.WithActor(context.Background(), app.Actor{UserID: domain.UserID("usr_owner")})
	saved, err := use.Run(ctx, testOrg, app.UpdatePolicyCommand{
		CurrencyRules: map[domain.Currency]policy.CurrencyRule{domain.USD: {MaxPerTransactionMinor: 5_000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.OrgID != testOrg || saved.CreatedBy != domain.UserID("usr_owner") {
		t.Errorf("saved = %+v, want it scoped to the org and attributed to the acting user", saved)
	}
	events := auditLog.All()
	if len(events) != 1 || events[0].Action != app.AuditPolicyUpdated {
		t.Errorf("audit events = %+v, want exactly one AuditPolicyUpdated", events)
	}
}

// A malformed edit - an unsupported currency, or a negative limit -
// must be rejected as a validation error, not silently saved with the
// bad currency dropped or the negative limit accepted as a ceiling
// nothing can pass.
func TestUpdatePolicy_RejectsAMalformedEdit(t *testing.T) {
	policies := &apptest.FixedPolicy{}
	clock := apptest.NewClock()
	use := &app.UpdatePolicy{Policies: policies, Clock: clock, Audit: &app.Audit{Log: apptest.NewAuditLog(), IDs: &apptest.IDs{}, Clock: clock}}
	ctx := app.WithActor(context.Background(), app.Actor{UserID: domain.UserID("usr_owner")})

	tests := map[string]app.UpdatePolicyCommand{
		"unsupported currency": {CurrencyRules: map[domain.Currency]policy.CurrencyRule{domain.Currency("JPY"): {MaxPerTransactionMinor: 1_000}}},
		"negative limit":       {CurrencyRules: map[domain.Currency]policy.CurrencyRule{domain.USD: {MaxPerTransactionMinor: -1}}},
	}
	for name, cmd := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := use.Run(ctx, testOrg, cmd)
			var verr *app.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Run with a %s = %v, want a *ValidationError", name, err)
			}
		})
	}
}

// Commerce has no escalation workflow yet - SubmitCheckout's interim
// shim treats Escalate exactly like Deny. Dry-run must report the
// same thing, or it drifts from what submission actually does.
func TestDryRunPolicy_CommerceEscalateReportsAsDeny(t *testing.T) {
	policies := &apptest.FixedPolicy{Outcome: policy.Escalate, Reasons: []policy.Reason{{Code: policy.ReasonOverApprovalThreshold}}}
	use := &app.DryRunPolicy{Policies: policies}

	d, err := use.Run(context.Background(), testOrg, app.DryRunPolicyCommand{Context: policy.ContextCommerce, Currency: domain.USD, AmountMinor: 1_000_000})
	if err != nil || d.Outcome != policy.Deny {
		t.Errorf("DryRunPolicy(commerce, Escalate) = %+v, %v, want it reported as Deny", d, err)
	}
}

// Treasury's own escalation workflow is real (Phase 6d), so an
// Escalate decision must be reported as-is there.
func TestDryRunPolicy_TreasuryEscalateReportsAsEscalate(t *testing.T) {
	policies := &apptest.FixedPolicy{Outcome: policy.Escalate, Reasons: []policy.Reason{{Code: policy.ReasonOverApprovalThreshold}}}
	use := &app.DryRunPolicy{Policies: policies}

	d, err := use.Run(context.Background(), testOrg, app.DryRunPolicyCommand{Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 1_000_000})
	if err != nil || d.Outcome != policy.Escalate {
		t.Errorf("DryRunPolicy(treasury, Escalate) = %+v, %v, want it reported as Escalate", d, err)
	}
}

func TestDryRunPolicy_EvaluatesWithoutError(t *testing.T) {
	policies := &apptest.FixedPolicy{Outcome: policy.Deny, Reasons: []policy.Reason{{Code: policy.ReasonOverTransactionLimit}}}
	use := &app.DryRunPolicy{Policies: policies}

	d, err := use.Run(context.Background(), testOrg, app.DryRunPolicyCommand{Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 1_000_000})
	if err != nil || d.Outcome != policy.Deny {
		t.Errorf("DryRunPolicy = %+v, %v, want the fixed Deny decision surfaced as-is", d, err)
	}
	if len(policies.Calls) != 1 {
		t.Errorf("policy evaluated %d times, want exactly 1", len(policies.Calls))
	}
}
