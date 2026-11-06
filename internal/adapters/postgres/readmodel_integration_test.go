//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestEventLog_MovesTheIntentsStateWithEachEvent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo, events := NewIntentRepository(pool), NewEventLog(pool)
	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)
	if _, _, err := repo.Save(ctx, intent); err != nil {
		t.Fatal(err)
	}

	state := func() (st, transfer, reason *string, changed *time.Time) {
		pool.QueryRow(ctx, `SELECT state, transfer_id, failure_reason, state_changed_at FROM intents WHERE key = $1`, string(intent.Key)).
			Scan(&st, &transfer, &reason, &changed)
		return
	}
	if st, _, _, _ := state(); st != nil {
		t.Errorf("state before any event = %q, want NULL (received)", *st)
	}

	for _, ev := range []struct{ typ, payload string }{{"PROPOSED", ""}, {"AUTHORIZED", ""}, {"IN_FLIGHT", ""}, {"APPLIED", "tr_123"}} {
		e, err := events.Append(ctx, intent.OrgID, intent.Key, ev.typ, ev.payload)
		if err != nil {
			t.Fatal(err)
		}
		st, transfer, _, changed := state()
		if st == nil || *st != ev.typ || changed == nil || !changed.Equal(e.RecordedAt) {
			t.Fatalf("after %s: state %v changed %v, want %s at %v", ev.typ, st, changed, ev.typ, e.RecordedAt)
		}
		if ev.typ == "APPLIED" && (transfer == nil || *transfer != "tr_123") {
			t.Errorf("transfer_id = %v, want tr_123", transfer)
		}
	}

	chain, _ := events.Load(ctx, intent.OrgID, intent.Key)
	for _, e := range chain {
		if e.RecordedAt.IsZero() {
			t.Errorf("event %d has no recorded time", e.SequenceNumber)
		}
	}
}

func TestEventLog_RefusesAnEventForAnIntentNotInTheOrganisation(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo, events := NewIntentRepository(pool), NewEventLog(pool)
	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)
	repo.Save(ctx, intent)
	other, _ := newTestOrg(t, NewAccountStore(pool))

	if _, err := events.Append(ctx, other.ID, intent.Key, "PROPOSED", ""); err == nil {
		t.Error("appended an event under another organisation")
	}
}

// Phase 1 left this open: concurrent appends to one intent could both
// compute the same next sequence number. The row lock queues them.
func TestEventLog_ConcurrentAppendsStayGapless(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo, events := NewIntentRepository(pool), NewEventLog(pool)
	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)
	repo.Save(ctx, intent)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := events.Append(ctx, intent.OrgID, intent.Key, "PROPOSED", ""); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent append: %v", err)
	}
	chain, _ := events.Load(ctx, intent.OrgID, intent.Key)
	if len(chain) != n {
		t.Fatalf("%d events stored, want %d", len(chain), n)
	}
	if err := chain.Verify(); err != nil {
		t.Errorf("chain after concurrent appends: %v", err)
	}
}

func TestIntentRepository_StoresTheSubmittingAgent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo := NewIntentRepository(pool)
	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)

	issued, err := newAgentFor(t, pool, intent.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	intent.Agent = issued
	stored, _, err := repo.Save(ctx, intent)
	if err != nil || stored.Agent != issued {
		t.Errorf("stored agent = %q, %v; want %q", stored.Agent, err, issued)
	}
}
