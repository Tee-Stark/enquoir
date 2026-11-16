//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/domain"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://enquoir:enquoir@localhost:5433/enquoir?sslmode=disable"
	}

	pool, err := NewPool(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connecting to Postgres at %s: %v", databaseURL, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testIntent builds an Intent for a freshly stored, real organisation -
// intents.org_id references organisations(id), so there is no
// shortcut around having one.
func testIntent(t *testing.T, pool *pgxpool.Pool) domain.Intent {
	t.Helper()
	org, _ := newTestOrg(t, NewAccountStore(pool))
	intent, err := domain.NewIntent(
		org.ID, treasuryOf(org.ID),
		fmt.Sprintf("payee-%s", t.Name()),
		10050, "USD",
		fmt.Sprintf("ref-%s", t.Name()),
		"integration test",
	)
	if err != nil {
		t.Fatalf("building test intent: %v", err)
	}
	return intent
}

func cleanupIntent(t *testing.T, pool *pgxpool.Pool, key domain.IntentKey) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM intent_events WHERE intent_key = $1`, string(key))
		pool.Exec(ctx, `DELETE FROM intents WHERE key = $1`, string(key))
	})
}

func TestIntentRepository_SavingSameKeyTwiceReturnsExistingIntent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo := NewIntentRepository(pool)

	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)

	first, created, err := repo.Save(ctx, intent)
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second, createdAgain, err := repo.Save(ctx, intent)
	if err != nil {
		t.Fatalf("second Save: %v", err)
	}

	if !created || createdAgain {
		t.Errorf("created = %v then %v, want true then false", created, createdAgain)
	}
	if first != second {
		t.Errorf("second Save returned a different intent: %+v vs %+v", first, second)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM intents WHERE key = $1`, string(intent.Key)).Scan(&count); err != nil {
		t.Fatalf("counting stored intents: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly one stored row for key %s, got %d", intent.Key, count)
	}
}

func TestEventLog_AppendsWithGaplessSequenceNumbers(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo := NewIntentRepository(pool)
	events := NewEventLog(pool)

	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)
	if _, _, err := repo.Save(ctx, intent); err != nil {
		t.Fatalf("saving intent: %v", err)
	}

	for wantSeq, eventType := range []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT"} {
		event, err := events.Append(ctx, intent.OrgID, intent.Key, eventType, "payload")
		if err != nil {
			t.Fatalf("appending event %d: %v", wantSeq, err)
		}
		if event.SequenceNumber != wantSeq {
			t.Errorf("event %d: got sequence number %d, want %d", wantSeq, event.SequenceNumber, wantSeq)
		}
	}
}

func TestEventLog_StoredChainVerifiesAfterReload(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo := NewIntentRepository(pool)
	events := NewEventLog(pool)

	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)
	if _, _, err := repo.Save(ctx, intent); err != nil {
		t.Fatalf("saving intent: %v", err)
	}

	for _, eventType := range []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT"} {
		if _, err := events.Append(ctx, intent.OrgID, intent.Key, eventType, "payload"); err != nil {
			t.Fatalf("appending %s: %v", eventType, err)
		}
	}

	reloaded, err := events.Load(ctx, intent.OrgID, intent.Key)
	if err != nil {
		t.Fatalf("loading chain: %v", err)
	}
	if len(reloaded) != 3 {
		t.Fatalf("expected 3 events, got %d", len(reloaded))
	}
	if err := reloaded.Verify(); err != nil {
		t.Errorf("reloaded chain failed to verify: %v", err)
	}
}

// Tenants are isolated: explicit org scoping must block a cross-tenant
// read even for a key that, by construction, could never arise under
// a different organisation - the point is the database-level guard,
// independent of the key's own uncollidability.
func TestEventLog_KeyFromAnotherOrganisationReturnsEmptyChain(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	repo := NewIntentRepository(pool)
	events := NewEventLog(pool)

	intent := testIntent(t, pool)
	cleanupIntent(t, pool, intent.Key)
	if _, _, err := repo.Save(ctx, intent); err != nil {
		t.Fatalf("saving intent: %v", err)
	}
	if _, err := events.Append(ctx, intent.OrgID, intent.Key, "PROPOSED", ""); err != nil {
		t.Fatalf("appending event: %v", err)
	}

	otherOrg, _ := newTestOrg(t, NewAccountStore(pool))
	chain, err := events.Load(ctx, otherOrg.ID, intent.Key)
	if err != nil {
		t.Fatalf("loading chain for another organisation: %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("another organisation saw %d events for a key it does not own, want 0", len(chain))
	}
}
