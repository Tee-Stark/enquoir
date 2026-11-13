package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type escalationFixture struct {
	accounts *apptest.Accounts
	agents   *apptest.Agents
	payments *apptest.Payments
	signaler *apptest.Signaler
	audit    *apptest.AuditLog
	decide   *app.DecideEscalatedPayment
	org      domain.OrgID
	creator  domain.UserID
	other    domain.UserID
}

// newEscalationFixture builds an org with one agent created by
// creator, one payment of that agent's waiting in AWAITING_APPROVAL,
// and two owners: creator and, unless soleOwner, other.
func newEscalationFixture(t *testing.T, soleOwner bool) *escalationFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	accounts := apptest.NewAccounts()
	org := domain.OrgID("org_esc")
	creator := app.User{ID: "usr_creator", Email: "creator@example.com", CreatedAt: now}
	other := app.User{ID: "usr_other", Email: "other@example.com", CreatedAt: now}

	if err := accounts.CreateOrganisationWithOwner(ctx, app.Organisation{ID: org, Name: "Test", CreatedAt: now}, creator); err != nil {
		t.Fatal(err)
	}
	// Registers other as a user (owner of a throwaway org), then adds
	// them to the real org - the fixture's equivalent of someone
	// signing up elsewhere and joining.
	if err := accounts.CreateOrganisationWithOwner(ctx, app.Organisation{ID: "org_throwaway", Name: "Throwaway", CreatedAt: now}, other); err != nil {
		t.Fatal(err)
	}
	if !soleOwner {
		accounts.AddMember(org, other.ID, domain.Owner, now)
	}

	agents := apptest.NewAgents()
	agentID := domain.AgentID("agt_1")
	if err := agents.CreateAgent(ctx, app.Agent{ID: agentID, OrgID: org, Name: "bot", CreatedAt: now, CreatedBy: creator.ID},
		app.APIKey{ID: "key_1", AgentID: agentID, OrgID: org, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	payments := apptest.NewPayments(app.Payment{Key: "key_1", OrgID: org, State: string(domain.AWAITING_APPROVAL), AgentID: agentID})
	signaler := apptest.NewSignaler()
	auditLog := apptest.NewAuditLog()

	return &escalationFixture{
		accounts: accounts, agents: agents, payments: payments, signaler: signaler, audit: auditLog,
		decide: &app.DecideEscalatedPayment{Payments: payments, Agents: agents, Members: accounts, Signaler: signaler, Audit: &app.Audit{Log: auditLog, IDs: &apptest.IDs{}, Clock: apptest.NewClock()}},
		org:    org, creator: creator.ID, other: other.ID,
	}
}

func asUser(ctx context.Context, user domain.UserID) context.Context {
	return app.WithActor(ctx, app.Actor{UserID: user})
}

func TestDecideEscalatedPayment_CreatorExcludedWhenAnotherOwnerExists(t *testing.T) {
	f := newEscalationFixture(t, false)
	err := f.decide.Run(asUser(context.Background(), f.creator), f.org, "key_1", true)
	if !errors.Is(err, app.ErrApproverExcluded) {
		t.Fatalf("creator approving with another owner available: err = %v, want ErrApproverExcluded", err)
	}
	if len(f.signaler.Calls()) != 0 {
		t.Error("a signal was sent despite the exclusion")
	}
}

func TestDecideEscalatedPayment_AnotherOwnerMayApprove(t *testing.T) {
	f := newEscalationFixture(t, false)
	if err := f.decide.Run(asUser(context.Background(), f.other), f.org, "key_1", true); err != nil {
		t.Fatal(err)
	}
	calls := f.signaler.Calls()
	if len(calls) != 1 || !calls[0].Approved || calls[0].DecidedBy != "usr_other" {
		t.Errorf("signal calls = %+v, want one approval by usr_other", calls)
	}
	events := f.audit.All()
	if len(events) != 1 || events[0].Action != app.AuditPaymentApproved {
		t.Errorf("audit = %+v, want one AuditPaymentApproved", events)
	}
}

// The exclusion is waived when the creator is the organisation's only
// eligible owner/admin - a confirmed product decision.
func TestDecideEscalatedPayment_ExclusionWaivedWhenCreatorIsSoleOwner(t *testing.T) {
	f := newEscalationFixture(t, true)
	if err := f.decide.Run(asUser(context.Background(), f.creator), f.org, "key_1", true); err != nil {
		t.Fatalf("sole owner approving their own agent's payment: %v, want it allowed", err)
	}
	if calls := f.signaler.Calls(); len(calls) != 1 || !calls[0].Approved {
		t.Errorf("signal calls = %+v, want one approval", calls)
	}
}

// Rejection is never excluded - it is not a conflict of interest the
// way approval is.
func TestDecideEscalatedPayment_CreatorMayAlwaysReject(t *testing.T) {
	f := newEscalationFixture(t, false)
	if err := f.decide.Run(asUser(context.Background(), f.creator), f.org, "key_1", false); err != nil {
		t.Fatalf("creator rejecting: %v, want it allowed", err)
	}
	if calls := f.signaler.Calls(); len(calls) != 1 || calls[0].Approved {
		t.Errorf("signal calls = %+v, want one rejection", calls)
	}
	events := f.audit.All()
	if len(events) != 1 || events[0].Action != app.AuditPaymentRejected {
		t.Errorf("audit = %+v, want one AuditPaymentRejected", events)
	}
}

// An admin is just as eligible an approver as an owner - the
// exclusion applies to the creator even when the only other eligible
// approver is an admin, not only when it's an owner.
func TestDecideEscalatedPayment_CreatorExclusionConsidersAdminToo(t *testing.T) {
	f := newEscalationFixture(t, false)
	f.accounts.AddMember(f.org, f.other, domain.Admin, time.Now())

	err := f.decide.Run(asUser(context.Background(), f.creator), f.org, "key_1", true)
	if !errors.Is(err, app.ErrApproverExcluded) {
		t.Fatalf("creator approving with an admin available: err = %v, want ErrApproverExcluded", err)
	}

	if err := f.decide.Run(asUser(context.Background(), f.other), f.org, "key_1", true); err != nil {
		t.Fatalf("the admin approving: %v, want it allowed", err)
	}
}

// Operators and viewers are not eligible approvers - their presence
// must never count towards "another eligible approver exists", so
// the exclusion stays waived for the creator exactly as if nobody
// else were in the organisation at all.
func TestDecideEscalatedPayment_OperatorsAndViewersDontCountAsEligibleApprovers(t *testing.T) {
	f := newEscalationFixture(t, true)
	f.accounts.AddMember(f.org, f.other, domain.Operator, time.Now())
	viewer := app.User{ID: "usr_viewer", Email: "viewer@example.com", CreatedAt: time.Now()}
	if err := f.accounts.CreateOrganisationWithOwner(context.Background(), app.Organisation{ID: "org_throwaway2", Name: "x", CreatedAt: time.Now()}, viewer); err != nil {
		t.Fatal(err)
	}
	f.accounts.AddMember(f.org, viewer.ID, domain.Viewer, time.Now())

	if err := f.decide.Run(asUser(context.Background(), f.creator), f.org, "key_1", true); err != nil {
		t.Fatalf("creator approving with only an operator and a viewer around: %v, want it allowed (neither is an eligible approver)", err)
	}
}

// An agent recorded before Agent.CreatedBy existed (6a) has no known
// creator - the exclusion can never apply to it, for anyone.
func TestDecideEscalatedPayment_NoRecordedCreator_ExclusionNeverApplies(t *testing.T) {
	f := newEscalationFixture(t, false)
	legacyAgent := domain.AgentID("agt_legacy")
	if err := f.agents.CreateAgent(context.Background(), app.Agent{ID: legacyAgent, OrgID: f.org, Name: "legacy-bot", CreatedAt: time.Now()},
		app.APIKey{ID: "key_legacy", AgentID: legacyAgent, OrgID: f.org, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.payments.Add(app.Payment{Key: "key_legacy_pay", OrgID: f.org, State: string(domain.AWAITING_APPROVAL), AgentID: legacyAgent})

	if err := f.decide.Run(asUser(context.Background(), f.creator), f.org, "key_legacy_pay", true); err != nil {
		t.Fatalf("approving a legacy agent's payment with no recorded creator: %v, want it allowed", err)
	}
}

func TestDecideEscalatedPayment_RefusesAPaymentNotAwaitingApproval(t *testing.T) {
	f := newEscalationFixture(t, false)
	f.payments.Add(app.Payment{Key: "key_2", OrgID: f.org, State: string(domain.AUTHORIZED)})
	err := f.decide.Run(asUser(context.Background(), f.other), f.org, "key_2", true)
	if !errors.Is(err, app.ErrNotAwaitingApproval) {
		t.Errorf("deciding an already-authorized payment: err = %v, want ErrNotAwaitingApproval", err)
	}
}
