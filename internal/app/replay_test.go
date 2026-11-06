package app_test

import (
	"strings"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// A payment that hit an unclear create, was looked up, retried and
// paid; then the agent submitted it again.
func replayFixture() app.PaymentDetail {
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	done := func(ms int) *time.Time { t := at(ms); return &t }
	key := domain.IntentKey("k1")

	var chain domain.Chain
	for _, e := range []struct {
		typ, payload string
		ms           int
	}{
		{"PROPOSED", "", 5}, {"AUTHORIZED", "", 10}, {"IN_FLIGHT", "", 15},
		{"UNKNOWN", "rail: outcome ambiguous", 60}, {"IN_FLIGHT", "", 80}, {"APPLIED", "tr_1", 200},
	} {
		chain = chain.Append(key, e.typ, e.payload)
		chain[len(chain)-1].RecordedAt = at(e.ms)
	}
	sub := func(id string, ms int, outcome app.AttemptOutcome) app.Attempt {
		return app.Attempt{ID: id, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent, Outcome: outcome,
			AgentID: "agt_1", AgentName: "Invoice bot", StartedAt: at(ms), FinishedAt: done(ms + 2), Details: map[string]string{"reported_state": "APPLIED"}}
	}
	call := func(id, op string, ms, dur int, outcome app.AttemptOutcome, attempt int) app.Attempt {
		return app.Attempt{ID: id, Kind: app.KindRailCall, Operation: op, Outcome: outcome, ActivityAttempt: attempt, StartedAt: at(ms), FinishedAt: done(ms + dur)}
	}
	create1 := call("a2", app.OpCreateTransfer, 20, 30, app.OutcomeAmbiguous, 1)
	create1.HTTPStatus = 503
	create2 := call("a4", app.OpCreateTransfer, 90, 40, app.OutcomeOK, 1)
	create2.TransferID, create2.TransferStatus = "tr_1", "PROCESSING"
	paid := call("a5", app.OpGetTransfer, 150, 20, app.OutcomeOK, 1)
	paid.TransferID, paid.TransferStatus = "tr_1", "PAID"

	return app.PaymentDetail{
		Payment: app.Payment{Key: key, State: "APPLIED", TransferID: "tr_1"},
		Events:  chain,
		Attempts: []app.Attempt{
			sub("a1", 0, app.OutcomeStarted),
			create1,
			call("a3", app.OpFindTransfer, 70, 5, app.OutcomeNotFound, 1),
			create2,
			paid,
			sub("a6", 900, app.OutcomeDeduplicated),
		},
	}
}

func TestBuildReplay_OrdersEveryStepInTime(t *testing.T) {
	r := app.BuildReplay(replayFixture())

	var got []string
	for _, s := range r.Steps {
		label := s.Type
		if s.ToState != "" {
			label += ":" + s.ToState
		} else if s.Attempt != nil {
			label += ":" + s.Attempt.Operation
		}
		got = append(got, label)
	}
	want := []string{
		"submission:submit_intent", "transition:PROPOSED", "transition:AUTHORIZED", "transition:IN_FLIGHT",
		"rail_call:create_transfer", "transition:UNKNOWN", "rail_call:find_transfer", "transition:IN_FLIGHT",
		"rail_call:create_transfer", "rail_call:get_transfer", "transition:APPLIED", "submission:submit_intent",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("steps:\n got %v\nwant %v", got, want)
	}
	for i, s := range r.Steps {
		if s.Index != i {
			t.Errorf("step %d has index %d", i, s.Index)
		}
		if s.Title == "" || s.Actor == "" {
			t.Errorf("step %d (%s) lacks a title or actor", i, s.Type)
		}
	}
}

func TestBuildReplay_TimesEachStep(t *testing.T) {
	r := app.BuildReplay(replayFixture())
	if r.DurationMS != 900 || !r.StartedAt.Equal(r.Steps[0].At) || !r.EndedAt.Equal(r.Steps[len(r.Steps)-1].At) {
		t.Errorf("duration %d, %v..%v", r.DurationMS, r.StartedAt, r.EndedAt)
	}
	unknown := r.Steps[5]
	if unknown.OffsetMS != 60 || unknown.DeltaMS != 40 {
		t.Errorf("UNKNOWN offset/delta = %d/%d, want 60/40", unknown.OffsetMS, unknown.DeltaMS)
	}
	if create := r.Steps[4]; create.DurationMS == nil || *create.DurationMS != 30 {
		t.Errorf("first create duration = %v, want 30", create.DurationMS)
	}
	if r.Steps[0].OffsetMS != 0 || r.Steps[0].DeltaMS != 0 {
		t.Error("the first step is not at offset 0")
	}
}

func TestBuildReplay_SaysWhoActedAndWhatHappened(t *testing.T) {
	r := app.BuildReplay(replayFixture())
	checks := []struct {
		i            int
		actor, from  string
		titleContain string
	}{
		{0, "agent", "", "Invoice bot"},
		{2, "policy", "PROPOSED", "olicy"},
		{4, "rail", "", "503"},
		{5, "workflow", "IN_FLIGHT", "unclear"},
		{6, "rail", "", "no transfer"},
		{8, "rail", "", "tr_1"},
		{10, "rail", "IN_FLIGHT", "paid"},
		{11, "agent", "", "duplicate"},
	}
	for _, c := range checks {
		s := r.Steps[c.i]
		if s.Actor != c.actor || s.FromState != c.from || !strings.Contains(s.Title, c.titleContain) {
			t.Errorf("step %d: actor %q from %q title %q; want %q, %q, containing %q", c.i, s.Actor, s.FromState, s.Title, c.actor, c.from, c.titleContain)
		}
	}
	if r.Steps[3].Event == nil || r.Steps[3].Event.SequenceNumber != 2 {
		t.Error("a transition does not carry its event")
	}
	if strings.Join(r.States, ">") != "PROPOSED>AUTHORIZED>IN_FLIGHT>UNKNOWN>IN_FLIGHT>APPLIED" {
		t.Errorf("states = %v", r.States)
	}
	if !r.ChainValid {
		t.Error("chain reported invalid")
	}
}

func TestBuildReplay_MarksRetriesAndInterruptedCalls(t *testing.T) {
	d := replayFixture()
	d.Attempts[3].ActivityAttempt = 2
	d.Attempts[1].Outcome, d.Attempts[1].FinishedAt = app.OutcomeInterrupted, nil
	r := app.BuildReplay(d)
	if !strings.Contains(r.Steps[8].Title, "retry") {
		t.Errorf("retried create title %q does not say retry", r.Steps[8].Title)
	}
	if s := r.Steps[4]; s.DurationMS != nil || !strings.Contains(s.Title, "never recorded") {
		t.Errorf("interrupted create: duration %v, title %q", s.DurationMS, s.Title)
	}
}

func TestBuildReplay_OfAPaymentNothingHasHappenedTo(t *testing.T) {
	r := app.BuildReplay(app.PaymentDetail{Payment: app.Payment{Key: "k", State: app.StateReceived}})
	if len(r.Steps) != 0 || r.DurationMS != 0 || !r.ChainValid {
		t.Errorf("empty replay = %+v", r)
	}
}
