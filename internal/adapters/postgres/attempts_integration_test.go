//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func TestAttemptLog_RecordStartFinishAndList(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	log := NewAttemptLog(pool)
	org, _ := newTestOrg(t, NewAccountStore(pool))
	other, _ := newTestOrg(t, NewAccountStore(pool))
	agent, err := newAgentFor(t, pool, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := pageTime(time.Now())
	key := domain.IntentKey(ids.NewID("k"))

	sub := app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent,
		Outcome: app.OutcomeStarted, IntentKey: key, AgentID: agent, Details: map[string]string{"amount": "10.50"},
		StartedAt: now.Add(-3 * time.Second), FinishedAt: ptr(now.Add(-3 * time.Second))}
	call := app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindRailCall, Operation: app.OpCreateTransfer,
		IntentKey: key, RequestID: string(key), ActivityAttempt: 1, StartedAt: now.Add(-2 * time.Second)}
	stale := app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindRailCall, Operation: app.OpCreateTransfer,
		IntentKey: key, StartedAt: now.Add(-10 * time.Minute)}
	foreign := app.Attempt{ID: ids.NewID("att"), OrgID: other.ID, Kind: app.KindRailCall, Operation: app.OpGetTransfer, StartedAt: now}

	for _, err := range []error{log.Record(ctx, sub), log.Start(ctx, call), log.Start(ctx, stale), log.Start(ctx, foreign)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Finish(ctx, org.ID, call.ID, app.AttemptFinish{Outcome: app.OutcomeOK, TransferID: "tr_1", TransferStatus: "PROCESSING", FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := log.Finish(ctx, other.ID, call.ID, app.AttemptFinish{Outcome: app.OutcomeRejected, FinishedAt: now}); err == nil {
		t.Error("finished another organisation's attempt")
	}

	page, err := log.List(ctx, org.ID, app.AttemptFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Attempts) != 3 {
		t.Fatalf("%d attempts listed, want org's 3", len(page.Attempts))
	}
	got := map[string]app.Attempt{}
	for _, a := range page.Attempts {
		got[a.ID] = a
	}
	if a := got[sub.ID]; a.AgentName != "test agent" || a.Details["amount"] != "10.50" || a.Outcome != app.OutcomeStarted {
		t.Errorf("submission = %+v", a)
	}
	if a := got[call.ID]; a.Outcome != app.OutcomeOK || a.TransferID != "tr_1" || a.FinishedAt == nil || a.ActivityAttempt != 1 {
		t.Errorf("finished call = %+v", a)
	}
	if a := got[stale.ID]; a.Outcome != app.OutcomeInterrupted {
		t.Errorf("a call unfinished for 10 minutes reads %q, want interrupted", a.Outcome)
	}
	if page.Attempts[0].ID != call.ID || page.Attempts[2].ID != stale.ID {
		t.Errorf("not newest first: %s, %s, %s", page.Attempts[0].ID, page.Attempts[1].ID, page.Attempts[2].ID)
	}

	for name, f := range map[string]app.AttemptFilter{
		"kind":        {Kind: app.KindAgentSubmission},
		"agent":       {AgentID: agent},
		"interrupted": {Outcomes: []app.AttemptOutcome{app.OutcomeInterrupted}},
	} {
		f.Limit = 10
		p, _ := log.List(ctx, org.ID, f)
		if len(p.Attempts) != 1 {
			t.Errorf("filter %s: %d attempts, want 1", name, len(p.Attempts))
		}
	}
	p, _ := log.List(ctx, org.ID, app.AttemptFilter{IntentKey: key, Kind: app.KindRailCall, Limit: 10})
	if len(p.Attempts) != 2 {
		t.Errorf("rail calls for the intent: %d, want 2", len(p.Attempts))
	}

	// Pages of one, followed by cursor, visit each attempt once.
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 5; i++ {
		p, err := log.List(ctx, org.ID, app.AttemptFilter{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range p.Attempts {
			if seen[a.ID] {
				t.Errorf("page %d repeats %s", i, a.ID)
			}
			seen[a.ID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 3 {
		t.Errorf("paging visited %d attempts, want 3", len(seen))
	}
	if _, err := log.List(ctx, org.ID, app.AttemptFilter{Limit: 1, Cursor: "garbage!"}); err == nil {
		t.Error("a garbage cursor was accepted")
	}
}

func ptr[T any](v T) *T { return &v }
