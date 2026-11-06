//go:build integration

package postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/domain"
)

// poolInFreshSchema returns a pool whose connections live in a new,
// empty Postgres schema, dropped when the test ends: a database at
// whatever migration state the test builds, isolated from the shared one.
func poolInFreshSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://enquoir:enquoir@localhost:5433/enquoir?sslmode=disable"
	}
	schema := strings.ReplaceAll(credentials.RandomIDs{}.NewID("migtest"), "-", "")

	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyUpTo runs the embedded migrations before (not including) stop,
// recording them as applied, the way an older deployment's database
// would look.
func applyUpTo(t *testing.T, pool *pgxpool.Pool, stop string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, _ := migrationsFS.ReadDir("migrations")
	for _, e := range entries {
		if e.Name() >= stop {
			return
		}
		sql, _ := migrationsFS.ReadFile("migrations/" + e.Name())
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("applying %s: %v", e.Name(), err)
		}
		pool.Exec(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, e.Name())
	}
}

// Recorded payment history is the audit trail: adding tenancy must not
// delete it. Intents recorded before organisations existed move to a
// "pre-tenancy" organisation, with their hash chains intact.
func TestMigration0005_KeepsPreTenancyHistory(t *testing.T) {
	ctx := context.Background()
	pool := poolInFreshSchema(t)
	applyUpTo(t, pool, "0005")

	const key = domain.IntentKey("0123456789abcdef0123456789abcdef")
	if _, err := pool.Exec(ctx, `
		INSERT INTO intents (key, payee, minor_units, currency, reference, reason)
		VALUES ($1, 'ben_old', 1050, 'USD', 'INV-OLD', 'before tenancy')`, string(key)); err != nil {
		t.Fatal(err)
	}
	var chain domain.Chain
	for _, typ := range []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED"} {
		chain = chain.Append(key, typ, "")
		e := chain[len(chain)-1]
		if _, err := pool.Exec(ctx, `
			INSERT INTO intent_events (intent_key, sequence_number, type, payload, prev_hash, hash)
			VALUES ($1, $2, $3, $4, $5, $6)`, string(key), e.SequenceNumber, e.Type, e.Payload, e.PrevHash, e.Hash); err != nil {
			t.Fatal(err)
		}
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrating a database holding pre-tenancy history: %v", err)
	}

	var org string
	if err := pool.QueryRow(ctx, `SELECT org_id FROM intents WHERE key = $1`, string(key)).Scan(&org); err != nil {
		t.Fatalf("the pre-tenancy intent is gone: %v", err)
	}
	if org != PreTenancyOrgID {
		t.Errorf("org_id = %q, want %q", org, PreTenancyOrgID)
	}
	var name string
	pool.QueryRow(ctx, `SELECT name FROM organisations WHERE id = $1`, org).Scan(&name)
	if name == "" {
		t.Error("the pre-tenancy organisation was not created")
	}

	loaded, err := NewEventLog(pool).Load(ctx, domain.OrgID(org), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 4 {
		t.Fatalf("history has %d events, want all 4", len(loaded))
	}
	if err := loaded.Verify(); err != nil {
		t.Errorf("the hash chain no longer verifies: %v", err)
	}
}

// A database with no history gets no pre-tenancy organisation.
func TestMigration0005_EmptyDatabaseGetsNoPreTenancyOrg(t *testing.T) {
	ctx := context.Background()
	pool := poolInFreshSchema(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM organisations`).Scan(&n)
	if n != 0 {
		t.Errorf("%d organisations after migrating an empty database, want 0", n)
	}
}
