package app_test

import (
	"context"
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
