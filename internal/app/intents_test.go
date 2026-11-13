package app_test

import (
	"context"
	"errors"
	"testing"

	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

const testOrg domain.OrgID = "org_test"

func testIntent(t *testing.T) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent(testOrg, "trs_test", "payee-1", 10050, "USD", "ref-"+t.Name(), "test")
	if err != nil {
		t.Fatalf("building test intent: %v", err)
	}
	return intent
}

func TestSubmitIntent_FreshSubmissionReturnsProposed(t *testing.T) {
	intent := testIntent(t)
	starter := &apptest.PaymentStarter{}
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.Key != intent.Key {
		t.Errorf("Key = %q, want %q", result.Key, intent.Key)
	}
	if result.State != domain.PROPOSED {
		t.Errorf("State = %q, want %q", result.State, domain.PROPOSED)
	}
	if starter.CallCount() != 1 {
		t.Errorf("Start called %d times, want 1", starter.CallCount())
	}
}

func TestSubmitIntent_AttachingReturnsCurrentRecordedState(t *testing.T) {
	intent := testIntent(t)
	events := apptest.NewEventLog()
	events.Append(context.Background(), testOrg, intent.Key, "PROPOSED", "")
	events.Append(context.Background(), testOrg, intent.Key, "AUTHORIZED", "")
	events.Append(context.Background(), testOrg, intent.Key, "APPLIED", "tr_1")

	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: &apptest.PaymentStarter{}, Events: events, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.State != domain.APPLIED {
		t.Errorf("State = %q, want %q (the already-recorded state, not a stale guess)", result.State, domain.APPLIED)
	}
}

func TestSubmitIntent_RecordedHistoryIsNeverRestarted(t *testing.T) {
	// A new run always begins at PROPOSED, which no recorded state may
	// legally move back to. Restarting an intent that already has
	// history - finished or not - would re-run the payment once the
	// workflow engine has forgotten the old run, so it must never
	// happen; a run still in progress carries on by itself.
	tests := []struct {
		name    string
		history []string
	}{
		{"applied", []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED"}},
		{"blocked", []string{"PROPOSED", "BLOCKED"}},
		{"failed", []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "FAILED"}},
		{"stuck unknown", []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "UNKNOWN"}},
		{"still in flight", []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := testIntent(t)
			events := apptest.NewEventLog()
			for _, typ := range tt.history {
				events.Append(context.Background(), testOrg, intent.Key, typ, "")
			}
			starter := &apptest.PaymentStarter{}
			use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: events, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

			result, err := use.Run(context.Background(), commandFor(intent))
			if err != nil {
				t.Fatalf("Run: unexpected error: %v", err)
			}
			if starter.CallCount() != 0 {
				t.Errorf("Start called %d times for an intent with recorded history, want 0", starter.CallCount())
			}
			if want := domain.State(tt.history[len(tt.history)-1]); result.State != want {
				t.Errorf("State = %q, want %q", result.State, want)
			}
		})
	}
}

func TestSubmitIntent_PropagatesEventLogError(t *testing.T) {
	// If the history can't be read, whether the intent already ran is
	// unknown - so nothing may be started.
	intent := testIntent(t)
	wantErr := errors.New("postgres unavailable")
	starter := &apptest.PaymentStarter{}
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: failingEventLog{err: wantErr}, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	_, err := use.Run(context.Background(), commandFor(intent))
	if !errors.Is(err, wantErr) {
		t.Errorf("Run error = %v, want it to wrap %v", err, wantErr)
	}
	if starter.CallCount() != 0 {
		t.Errorf("Start called %d times with an unreadable history, want 0", starter.CallCount())
	}
}

type failingEventLog struct{ err error }

func (l failingEventLog) Append(ctx context.Context, org domain.OrgID, key domain.IntentKey, eventType, payload string) (domain.Event, error) {
	return domain.Event{}, l.err
}

func (l failingEventLog) Load(ctx context.Context, org domain.OrgID, key domain.IntentKey) (domain.Chain, error) {
	return nil, l.err
}

func TestSubmitIntent_PropagatesStarterError(t *testing.T) {
	intent := testIntent(t)
	wantErr := errors.New("temporal unavailable")
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: &apptest.PaymentStarter{Err: wantErr}, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	_, err := use.Run(context.Background(), commandFor(intent))
	if !errors.Is(err, wantErr) {
		t.Errorf("Run error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestGetIntent_ReturnsStateAndHistory(t *testing.T) {
	intent := testIntent(t)
	events := apptest.NewEventLog()
	events.Append(context.Background(), testOrg, intent.Key, "PROPOSED", "")
	events.Append(context.Background(), testOrg, intent.Key, "AUTHORIZED", "")

	use := &app.GetIntent{Events: events}
	result, err := use.Run(context.Background(), testOrg, intent.Key)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.State != domain.AUTHORIZED {
		t.Errorf("State = %q, want %q", result.State, domain.AUTHORIZED)
	}
	if len(result.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2", len(result.Events))
	}
	if err := result.Events.Verify(); err != nil {
		t.Errorf("event chain failed to verify: %v", err)
	}
}

func TestGetIntent_UnknownKeyReturnsNotFound(t *testing.T) {
	use := &app.GetIntent{Events: apptest.NewEventLog()}
	_, err := use.Run(context.Background(), testOrg, domain.IntentKey("never-submitted"))
	if !errors.Is(err, app.ErrIntentNotFound) {
		t.Errorf("Run error = %v, want ErrIntentNotFound", err)
	}
}

// Tenants are isolated: a key recorded under one organisation must
// never be visible to another, even if somehow asked for by its exact
// value.
func TestGetIntent_KeyFromAnotherOrganisationReturnsNotFound(t *testing.T) {
	intent := testIntent(t)
	events := apptest.NewEventLog()
	events.Append(context.Background(), testOrg, intent.Key, "PROPOSED", "")

	use := &app.GetIntent{Events: events}
	_, err := use.Run(context.Background(), domain.OrgID("org_other"), intent.Key)
	if !errors.Is(err, app.ErrIntentNotFound) {
		t.Errorf("Run error = %v, want ErrIntentNotFound for another organisation's key", err)
	}
}

// ownedPayees is a store in which testOrg owns "payee-1", the payee
// testIntent pays.
func ownedPayees() *apptest.Beneficiaries {
	return apptest.NewBeneficiaries().Own(testOrg, "payee-1")
}

// Every organisation pays through one rail account, so a payee ID
// from another organisation is a real beneficiary the rail would pay.
// Only payees the submitting organisation registered may be paid.
func TestSubmitIntent_RefusesAPayeeTheOrganisationDoesNotOwn(t *testing.T) {
	intent := testIntent(t)
	cases := map[string]*apptest.Beneficiaries{
		"another organisation's payee": apptest.NewBeneficiaries().Own("org_other", "payee-1"),
		"a payee nobody registered":    apptest.NewBeneficiaries(),
	}
	for name, payees := range cases {
		t.Run(name, func(t *testing.T) {
			starter, events := &apptest.PaymentStarter{}, apptest.NewEventLog()
			use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: events, Payees: payees, Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

			_, err := use.Run(context.Background(), commandFor(intent))
			if !errors.Is(err, app.ErrPayeeUnknown) {
				t.Fatalf("err = %v, want ErrPayeeUnknown", err)
			}
			if starter.CallCount() != 0 {
				t.Errorf("started %d workflows for a payee the organisation does not own, want 0", starter.CallCount())
			}
		})
	}
}

// The API records the intent, and which agent submitted it, as soon as
// it is accepted: the dashboard shows a payment even before a worker
// picks it up, and a later submission by another agent of the same
// organisation does not change who submitted it first.
func TestSubmitIntent_RecordsTheIntentAndTheAgentThatFirstSubmittedIt(t *testing.T) {
	intent := testIntent(t)
	intent.Agent = "agt_first"
	intents := apptest.NewIntents()
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: &apptest.PaymentStarter{}, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: intents, Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	if _, err := use.Run(context.Background(), commandFor(intent)); err != nil {
		t.Fatal(err)
	}
	again := intent
	again.Agent = "agt_second"
	if _, err := use.Run(context.Background(), commandFor(again)); err != nil {
		t.Fatal(err)
	}

	stored, ok := intents.Get(intent.Key)
	if !ok || stored.Agent != "agt_first" || stored.OrgID != testOrg {
		t.Errorf("stored = %+v (found %v), want the intent attributed to agt_first", stored, ok)
	}
}

// A policy denial never starts the workflow or saves the intent - the
// same "the rail was never called" semantics as a payee unknown error.
// A retry of an intent that already has history (settled or still in
// flight) must be recognised as a duplicate before policy is ever
// re-evaluated - otherwise a retry near a limit could be spuriously
// denied (or re-escalated, re-emailing every approver) for a payment
// that already went through.
func TestSubmitIntent_RetryOfAlreadyRecordedIntent_NeverReEvaluatesPolicy(t *testing.T) {
	intent := testIntent(t)
	events := apptest.NewEventLog()
	events.Append(context.Background(), testOrg, intent.Key, "PROPOSED", "")
	events.Append(context.Background(), testOrg, intent.Key, "AUTHORIZED", "")
	events.Append(context.Background(), testOrg, intent.Key, "APPLIED", "tr_1")

	starter := &apptest.PaymentStarter{}
	policies := &apptest.FixedPolicy{Outcome: policy.Deny, Reasons: []policy.Reason{{Code: policy.ReasonOverTransactionLimit}}}
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: events, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock(), Policies: policies}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil {
		t.Fatalf("retrying an already-applied intent: %v, want the deduplicated result, not a policy denial", err)
	}
	if result.State != domain.APPLIED || result.Outcome != app.OutcomeDeduplicated {
		t.Errorf("result = %+v, want the already-recorded APPLIED/deduplicated result", result)
	}
	if len(policies.Calls) != 0 {
		t.Errorf("policy evaluated %d times on a retry with existing history, want 0", len(policies.Calls))
	}
	if starter.CallCount() != 0 {
		t.Errorf("started %d workflows for a retry, want 0", starter.CallCount())
	}
}

func TestSubmitIntent_DeniedByPolicy_NeverStartsWorkflowOrSavesIntent(t *testing.T) {
	intent := testIntent(t)
	starter, intents := &apptest.PaymentStarter{}, apptest.NewIntents()
	policies := &apptest.FixedPolicy{Outcome: policy.Deny, Reasons: []policy.Reason{{Code: policy.ReasonOverTransactionLimit, Detail: "too much"}}, Version: 3}
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: intents, Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock(), Policies: policies}

	_, err := use.Run(context.Background(), commandFor(intent))
	var denied *app.PolicyDeniedError
	if !errors.As(err, &denied) || denied.Decision.Outcome != policy.Deny {
		t.Fatalf("err = %v, want a *PolicyDeniedError", err)
	}
	if starter.CallCount() != 0 {
		t.Errorf("started %d workflows for a policy-denied intent, want 0", starter.CallCount())
	}
	if _, ok := intents.Get(intent.Key); ok {
		t.Error("a policy-denied intent was saved; want it never persisted")
	}
}

// Escalate is treated identically to Deny until the real escalation
// state exists (6d) - this is an explicit interim shim, not the final
// behaviour, so the test name says so.
// Escalate starts the workflow normally - unlike Deny, which never
// does - but tells it to wait for an approval decision before
// authorizing. The HTTP response still reports PROPOSED, exactly like
// a payment that will shortly be BLOCKED by the hard ceiling inside
// the workflow: the real outcome is discovered by reading the payment
// back, not synchronously at submission.
func TestSubmitIntent_EscalatedByPolicy_StartsAndTellsTheWorkflowToWait(t *testing.T) {
	intent := testIntent(t)
	starter := &apptest.PaymentStarter{}
	policies := &apptest.FixedPolicy{Outcome: policy.Escalate, Reasons: []policy.Reason{{Code: policy.ReasonOverApprovalThreshold}}, Version: 1}
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock(), Policies: policies}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil {
		t.Fatalf("escalated submission: %v, want no error - escalation is not a rejection", err)
	}
	if result.State != domain.PROPOSED || result.Outcome != app.OutcomeStarted {
		t.Errorf("result = %+v, want a normal PROPOSED/started response", result)
	}
	if escalated := starter.Escalated(); len(escalated) != 1 || !escalated[0] {
		t.Errorf("Starter.Start escalate flags = %v, want exactly one true", escalated)
	}
}

func TestSubmitIntent_EscalatedByPolicy_NotifiesEveryOwnerAndAdminOnly(t *testing.T) {
	intent := testIntent(t)
	accounts := apptest.NewAccounts()
	owner := app.User{ID: "usr_owner", Email: "owner@example.com", CreatedAt: time.Now()}
	if err := accounts.CreateOrganisationWithOwner(context.Background(), app.Organisation{ID: testOrg, Name: "Test", CreatedAt: time.Now()}, owner); err != nil {
		t.Fatal(err)
	}
	viewer := app.User{ID: "usr_viewer", Email: "viewer@example.com", CreatedAt: time.Now()}
	if err := accounts.CreateOrganisationWithOwner(context.Background(), app.Organisation{ID: "org_throwaway", Name: "x", CreatedAt: time.Now()}, viewer); err != nil {
		t.Fatal(err)
	}
	accounts.AddMember(testOrg, viewer.ID, domain.Viewer, time.Now())

	outbox := apptest.NewOutbox()
	emails := &app.Emails{Outbox: outbox, Dispatcher: &apptest.Dispatcher{}, IDs: &apptest.IDs{}, Clock: apptest.NewClock()}
	policies := &apptest.FixedPolicy{Outcome: policy.Escalate, Reasons: []policy.Reason{{Code: policy.ReasonOverApprovalThreshold}}}
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: &apptest.PaymentStarter{}, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock(), Policies: policies, Members: accounts, Emails: emails}

	if _, err := use.Run(context.Background(), commandFor(intent)); err != nil {
		t.Fatal(err)
	}
	sent := outbox.All()
	if len(sent) != 1 || sent[0].Template != app.TemplateEscalationCreated || sent[0].To != owner.Email || sent[0].Link != nil {
		t.Errorf("sent = %+v, want exactly one no-link escalation_created email to the owner (never the viewer)", sent)
	}
}

func TestSubmitIntent_AllowedByPolicy_StartsNormallyAndPassesTheRightInput(t *testing.T) {
	intent := testIntent(t)
	starter := &apptest.PaymentStarter{}
	policies := &apptest.FixedPolicy{Outcome: policy.Allow, Version: 2}
	intents := apptest.NewIntents()
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: intents, Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock(), Policies: policies}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != domain.PROPOSED || starter.CallCount() != 1 {
		t.Errorf("result = %+v, started %d times; want a normal PROPOSED start", result, starter.CallCount())
	}
	if len(policies.Calls) != 1 {
		t.Fatalf("policy evaluated %d times, want 1", len(policies.Calls))
	}
	in := policies.Calls[0]
	if in.Context != policy.ContextTreasury || in.Currency != intent.Amount.Currency() || in.AmountMinor != intent.Amount.MinorUnits() {
		t.Errorf("evaluation input = %+v, want context=treasury, currency/amount matching the intent", in)
	}
	if in.Payee == nil || *in.Payee != intent.Payee {
		t.Errorf("evaluation input payee = %v, want %q", in.Payee, intent.Payee)
	}
	// Allow must record which version decided it too, not just
	// Deny/Escalate's audit entries - AGENTS.md's Phase 6 requires
	// every decision to record the version it used.
	stored, ok := intents.Get(intent.Key)
	if !ok || stored.PolicyVersion == nil || *stored.PolicyVersion != 2 {
		t.Errorf("saved intent's policy version = %v, ok=%v; want 2", stored.PolicyVersion, ok)
	}
}

// SubmitIntent works exactly as before when Policies is left unset -
// existing callers that have no stake in policy evaluation (and the
// many existing tests in this file) don't need to be updated.
func TestSubmitIntent_WithNoPolicyStoreConfigured_AlwaysAllows(t *testing.T) {
	intent := testIntent(t)
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: &apptest.PaymentStarter{}, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil || result.State != domain.PROPOSED {
		t.Errorf("result = %+v, %v, want a normal PROPOSED start with no Policies configured", result, err)
	}
}

// commandFor is the submission an agent would send for intent.
func commandFor(intent domain.Intent) app.SubmitIntentCommand {
	return app.SubmitIntentCommand{
		OrgID: intent.OrgID, Agent: intent.Agent, Payee: intent.Payee,
		Amount: intent.Amount.MajorUnitsString(), Currency: string(intent.Amount.Currency()),
		Reference: intent.Reference, Reason: intent.Reason,
	}
}

// An intent recorded before treasuries has its key without one. A
// resubmission of it is the same payment: it is reported as it stands,
// under that key, and nothing new starts - a new key would mean a new
// rail request_id, and the invoice paid twice.
func TestSubmitIntent_AnIntentRecordedBeforeTreasuriesIsNotPaidAgain(t *testing.T) {
	intent := testIntent(t)
	events := apptest.NewEventLog()
	events.Append(context.Background(), testOrg, intent.LegacyKey(), "PROPOSED", "")
	events.Append(context.Background(), testOrg, intent.LegacyKey(), "APPLIED", "tr_1")
	starter, intents := &apptest.PaymentStarter{}, apptest.NewIntents()
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: starter, Events: events, Payees: ownedPayees(), Intents: intents, Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

	result, err := use.Run(context.Background(), commandFor(intent))
	if err != nil {
		t.Fatal(err)
	}
	if result.Key != intent.LegacyKey() || result.State != domain.APPLIED || result.Outcome != app.OutcomeDeduplicated || starter.CallCount() != 0 {
		t.Errorf("result = %+v, %d starts; want the recorded intent, deduplicated, nothing started", result, starter.CallCount())
	}
	// Another organisation's identical submission is its own payment.
	other := commandFor(intent)
	other.OrgID = "org_other"
	use.Payees = apptest.NewBeneficiaries().Own("org_other", "payee-1")
	if r, _ := use.Run(context.Background(), other); r.Outcome != app.OutcomeStarted {
		t.Errorf("another organisation: %+v", r)
	}
}

// The submission is scoped to the organisation's treasury: its key
// includes it, and only a payee that treasury registered may be paid.
func TestSubmitIntent_IsScopedToTheTreasury(t *testing.T) {
	intent := testIntent(t)
	payees := apptest.NewBeneficiaries().OwnIn(testOrg, "trs_other", "payee-1")
	use := &app.SubmitIntent{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Starter: &apptest.PaymentStarter{}, Events: apptest.NewEventLog(), Payees: payees, Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}
	if _, err := use.Run(context.Background(), commandFor(intent)); !errors.Is(err, app.ErrPayeeUnknown) {
		t.Errorf("another treasury's payee: %v", err)
	}
	payees.OwnIn(testOrg, "trs_test", "payee-1")
	r, err := use.Run(context.Background(), commandFor(intent))
	if err != nil || r.Key != intent.Key {
		t.Errorf("result = %+v, %v; want the key with the treasury", r, err)
	}
}
