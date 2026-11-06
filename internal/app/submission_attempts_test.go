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

type submitFixture struct {
	use      *app.SubmitIntent
	attempts *apptest.Attempts
	intents  *apptest.Intents
	events   *apptest.EventLog
	starter  *apptest.PaymentStarter
	clock    *apptest.Clock
}

func newSubmitFixture() *submitFixture {
	f := &submitFixture{attempts: apptest.NewAttempts(), intents: apptest.NewIntents(), events: apptest.NewEventLog(), starter: &apptest.PaymentStarter{}, clock: apptest.NewClock()}
	f.use = &app.SubmitIntent{Starter: f.starter, Events: f.events, Payees: ownedPayees(), Intents: f.intents, Attempts: f.attempts, IDs: &apptest.IDs{}, Clock: f.clock}
	return f
}

var validCommand = app.SubmitIntentCommand{OrgID: testOrg, Agent: "agt_1", Payee: "payee-1", Amount: "100.50", Currency: "usd", Reference: "INV-9", Reason: "invoice"}

// Every submission, whatever happens to it, is one agent_submission
// attempt saying what the agent sent and what Enquoir did with it.
func TestSubmitIntent_RecordsEverySubmissionAsAnAttempt(t *testing.T) {
	key := func() domain.IntentKey {
		i, _ := domain.NewIntent(testOrg, "payee-1", 10050, "USD", "INV-9", "")
		return i.Key
	}()

	cases := []struct {
		name    string
		prepare func(f *submitFixture)
		cmd     app.SubmitIntentCommand
		outcome app.AttemptOutcome
		wantKey bool
		wantErr bool
	}{
		{"a new payment is started", nil, validCommand, app.OutcomeStarted, true, false},
		{"a run already starting is attached to", func(f *submitFixture) {
			i, _ := domain.NewIntent(testOrg, "payee-1", 10050, "USD", "INV-9", "")
			f.intents.Save(context.Background(), i)
		}, validCommand, app.OutcomeAttached, true, false},
		{"recorded history is deduplicated", func(f *submitFixture) {
			f.events.Append(context.Background(), testOrg, key, "PROPOSED", "")
		}, validCommand, app.OutcomeDeduplicated, true, false},
		{"invalid input", nil, func() app.SubmitIntentCommand { c := validCommand; c.Amount = "10.505"; return c }(), app.OutcomeInvalid, false, true},
		{"a payee the organisation does not own", nil, func() app.SubmitIntentCommand { c := validCommand; c.Payee = "payee-2"; return c }(), app.OutcomePayeeUnknown, true, true},
		{"Enquoir fails to start it", func(f *submitFixture) { f.starter.Err = errors.New("temporal down") }, validCommand, app.OutcomeError, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubmitFixture()
			if tc.prepare != nil {
				tc.prepare(f)
			}
			result, err := f.use.Run(context.Background(), tc.cmd)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && result.Outcome != tc.outcome {
				t.Errorf("result outcome = %q, want %q", result.Outcome, tc.outcome)
			}

			all := f.attempts.All()
			if len(all) != 1 {
				t.Fatalf("%d attempts recorded, want exactly 1", len(all))
			}
			a := all[0]
			if a.Kind != app.KindAgentSubmission || a.Operation != app.OpSubmitIntent || a.Outcome != tc.outcome {
				t.Errorf("attempt = %s/%s/%s, want agent_submission/submit_intent/%s", a.Kind, a.Operation, a.Outcome, tc.outcome)
			}
			if a.OrgID != testOrg || a.AgentID != "agt_1" || a.ID == "" {
				t.Errorf("attempt org/agent/id = %q/%q/%q", a.OrgID, a.AgentID, a.ID)
			}
			want, _ := domain.NewIntent(testOrg, tc.cmd.Payee, 10050, "USD", tc.cmd.Reference, "")
			if (a.IntentKey != "") != tc.wantKey || (tc.wantKey && a.IntentKey != want.Key) {
				t.Errorf("intent key = %q, want key present = %v", a.IntentKey, tc.wantKey)
			}
			if a.Details["amount"] != tc.cmd.Amount || a.Details["reference"] != "INV-9" {
				t.Errorf("details = %v, want what the agent sent", a.Details)
			}
			if a.FinishedAt == nil {
				t.Error("the attempt was not marked finished")
			}
			if tc.wantErr && a.ErrorMessage == "" {
				t.Error("a failed submission has no error message")
			}
		})
	}
}

// An internal error is recorded without its internals: the attempt is
// shown to the organisation, the error may name infrastructure.
func TestSubmitIntent_InternalErrorsAreNotExposedInTheAttempt(t *testing.T) {
	f := newSubmitFixture()
	f.starter.Err = errors.New("dial tcp 10.0.0.5:7233: connection refused")
	f.use.Run(context.Background(), validCommand)
	if msg := f.attempts.All()[0].ErrorMessage; msg == "" || msg == f.starter.Err.Error() {
		t.Errorf("error message = %q, want a generic message", msg)
	}
}

func TestSubmitIntent_ValidationNamesTheField(t *testing.T) {
	cases := map[string]func(c *app.SubmitIntentCommand){
		"payee":     func(c *app.SubmitIntentCommand) { c.Payee = " " },
		"currency":  func(c *app.SubmitIntentCommand) { c.Currency = "JPY" },
		"amount":    func(c *app.SubmitIntentCommand) { c.Amount = "0.00" },
		"reference": func(c *app.SubmitIntentCommand) { c.Reference = "" },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			cmd := validCommand
			mutate(&cmd)
			_, err := newSubmitFixture().use.Run(context.Background(), cmd)
			var verr *app.ValidationError
			if !errors.As(err, &verr) || verr.Field != field {
				t.Errorf("err = %v, want a ValidationError on %q", err, field)
			}
		})
	}
}

func TestSubmitIntent_AttemptTimesComeFromTheClock(t *testing.T) {
	f := newSubmitFixture()
	start := f.clock.Now()
	f.use.Run(context.Background(), validCommand)
	a := f.attempts.All()[0]
	if !a.StartedAt.Equal(start) || a.Duration() == nil || *a.Duration() != time.Duration(0) {
		t.Errorf("started %v, duration %v", a.StartedAt, a.Duration())
	}
}
