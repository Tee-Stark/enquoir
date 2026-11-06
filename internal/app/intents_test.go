package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

const testOrg domain.OrgID = "org_test"

func testIntent(t *testing.T) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent(testOrg, "payee-1", 10050, "USD", "ref-"+t.Name(), "test")
	if err != nil {
		t.Fatalf("building test intent: %v", err)
	}
	return intent
}

func TestSubmitIntent_FreshSubmissionReturnsProposed(t *testing.T) {
	intent := testIntent(t)
	starter := &apptest.PaymentStarter{}
	use := &app.SubmitIntent{Starter: starter, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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

	use := &app.SubmitIntent{Starter: &apptest.PaymentStarter{}, Events: events, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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
			use := &app.SubmitIntent{Starter: starter, Events: events, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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
	use := &app.SubmitIntent{Starter: starter, Events: failingEventLog{err: wantErr}, Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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
	use := &app.SubmitIntent{Starter: &apptest.PaymentStarter{Err: wantErr}, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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
			use := &app.SubmitIntent{Starter: starter, Events: events, Payees: payees, Intents: apptest.NewIntents(), Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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
	use := &app.SubmitIntent{Starter: &apptest.PaymentStarter{}, Events: apptest.NewEventLog(), Payees: ownedPayees(), Intents: intents, Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}

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

// commandFor is the submission an agent would send for intent.
func commandFor(intent domain.Intent) app.SubmitIntentCommand {
	return app.SubmitIntentCommand{
		OrgID: intent.OrgID, Agent: intent.Agent, Payee: intent.Payee,
		Amount: intent.Amount.MajorUnitsString(), Currency: string(intent.Amount.Currency()),
		Reference: intent.Reference, Reason: intent.Reason,
	}
}
