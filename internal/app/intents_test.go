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
	use := &app.SubmitIntent{Starter: starter, Events: apptest.NewEventLog()}

	result, err := use.Run(context.Background(), intent)
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

	use := &app.SubmitIntent{Starter: &apptest.PaymentStarter{}, Events: events}

	result, err := use.Run(context.Background(), intent)
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
			use := &app.SubmitIntent{Starter: starter, Events: events}

			result, err := use.Run(context.Background(), intent)
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
	use := &app.SubmitIntent{Starter: starter, Events: failingEventLog{err: wantErr}}

	_, err := use.Run(context.Background(), intent)
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
	use := &app.SubmitIntent{Starter: &apptest.PaymentStarter{Err: wantErr}, Events: apptest.NewEventLog()}

	_, err := use.Run(context.Background(), intent)
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
