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

func testIntent(t *testing.T) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent(
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

	intent := testIntent(t)
	cleanupIntent(t, pool, intent.Key)

	first, err := repo.Save(ctx, intent)
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second, err := repo.Save(ctx, intent)
	if err != nil {
		t.Fatalf("second Save: %v", err)
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

	intent := testIntent(t)
	cleanupIntent(t, pool, intent.Key)
	if _, err := repo.Save(ctx, intent); err != nil {
		t.Fatalf("saving intent: %v", err)
	}

	for wantSeq, eventType := range []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT"} {
		event, err := events.Append(ctx, intent.Key, eventType, "payload")
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

	intent := testIntent(t)
	cleanupIntent(t, pool, intent.Key)
	if _, err := repo.Save(ctx, intent); err != nil {
		t.Fatalf("saving intent: %v", err)
	}

	for _, eventType := range []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT"} {
		if _, err := events.Append(ctx, intent.Key, eventType, "payload"); err != nil {
			t.Fatalf("appending %s: %v", eventType, err)
		}
	}

	reloaded, err := events.Load(ctx, intent.Key)
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
