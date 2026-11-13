package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

var _ policy.Store = (*PolicyStore)(nil)

// PolicyStore persists policy versions and evaluates candidate
// payments against an organisation's current one.
type PolicyStore struct{ pool *pgxpool.Pool }

func NewPolicyStore(pool *pgxpool.Pool) *PolicyStore { return &PolicyStore{pool: pool} }

// rowQuerier is what both *pgxpool.Pool and pgx.Tx satisfy, so the
// same read can run standalone or inside a transaction already
// holding the organisation's lock.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// currentPolicy reads with q, so EvaluateAndReserve and SaveNewVersion
// can read the current version under the same transaction as their
// lock, never a second, separate one.
func currentPolicy(ctx context.Context, q rowQuerier, org domain.OrgID) (policy.PolicyVersion, error) {
	var canonical string
	err := q.QueryRow(ctx, `SELECT canonical FROM policy_versions WHERE org_id = $1 ORDER BY version DESC LIMIT 1`,
		string(org)).Scan(&canonical)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.DefaultPolicyVersion(org), nil
	}
	if err != nil {
		return policy.PolicyVersion{}, err
	}
	v, err := policy.ParsePolicyVersion([]byte(canonical))
	if err != nil {
		return policy.PolicyVersion{}, err
	}
	v.OrgID = org
	return v, nil
}

func (s *PolicyStore) CurrentPolicy(ctx context.Context, org domain.OrgID) (policy.PolicyVersion, error) {
	return currentPolicy(ctx, s.pool, org)
}

// lockOrg locks org's policy row for the rest of tx. app.ErrNotFound
// if the organisation itself is unknown.
func lockOrg(ctx context.Context, tx pgx.Tx, org domain.OrgID) error {
	var locked bool
	err := tx.QueryRow(ctx, `SELECT true FROM organisation_policy_locks WHERE org_id = $1 FOR UPDATE`, string(org)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ErrNotFound
	}
	return err
}

func (s *PolicyStore) SaveNewVersion(ctx context.Context, v policy.PolicyVersion) (policy.PolicyVersion, error) {
	var saved policy.PolicyVersion
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOrg(ctx, tx, v.OrgID); err != nil {
			return err
		}
		current, err := currentPolicy(ctx, tx, v.OrgID)
		if err != nil {
			return err
		}
		next := policy.PolicyVersion{
			OrgID: v.OrgID, Version: current.Version + 1,
			CurrencyRules: v.CurrencyRules, PayeeAllow: v.PayeeAllow, PayeeBlock: v.PayeeBlock, CategoryBlock: v.CategoryBlock,
			CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy,
		}
		id := credentials.RandomIDs{}.NewID("plv")
		if _, err := tx.Exec(ctx, `
			INSERT INTO policy_versions (id, org_id, version, canonical, hash, created_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, string(next.OrgID), next.Version, string(next.Canonical()), next.Hash(), next.CreatedAt, nullable(string(next.CreatedBy))); err != nil {
			return err
		}
		saved = next
		return nil
	})
	return saved, err
}

// rollingUsage is an organisation's usage across BOTH bounded
// contexts, summed in Go: one rolling total, not two separate ones,
// matching how commerce checkouts count the same way treasury
// payments do (AGENTS.md's Phase 6).
func rollingUsage(ctx context.Context, tx pgx.Tx, org domain.OrgID, currency domain.Currency, now time.Time) (policy.Usage, error) {
	t, err := treasuryUsage(ctx, tx, org, currency, now)
	if err != nil {
		return policy.Usage{}, err
	}
	c, err := commerceUsage(ctx, tx, org, currency, now)
	if err != nil {
		return policy.Usage{}, err
	}
	return policy.Usage{
		Last24h:       t.Last24h + c.Last24h,
		Last7d:        t.Last7d + c.Last7d,
		Last30d:       t.Last30d + c.Last30d,
		LastHourCount: t.LastHourCount + c.LastHourCount,
	}, nil
}

// treasuryUsage counts intents that reserved or spent their amount:
// PROPOSED (the state Intents.Save itself writes, the instant a payment
// is persisted - see policy.Store's doc comment on what "persisting"
// counting on means), AWAITING_APPROVAL/AUTHORIZED/IN_FLIGHT/UNKNOWN
// (held) and APPLIED (spent). BLOCKED/FAILED released, never counted.
func treasuryUsage(ctx context.Context, tx pgx.Tx, org domain.OrgID, currency domain.Currency, now time.Time) (policy.Usage, error) {
	var u policy.Usage
	err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(sum(minor_units) FILTER (WHERE state_changed_at > $3::timestamptz - interval '24 hours'), 0),
			COALESCE(sum(minor_units) FILTER (WHERE state_changed_at > $3::timestamptz - interval '7 days'), 0),
			COALESCE(sum(minor_units) FILTER (WHERE state_changed_at > $3::timestamptz - interval '30 days'), 0),
			count(*) FILTER (WHERE state_changed_at > $3::timestamptz - interval '1 hour')
		FROM intents
		WHERE org_id = $1 AND currency = $2 AND state IN ('PROPOSED', 'AWAITING_APPROVAL', 'AUTHORIZED', 'IN_FLIGHT', 'UNKNOWN', 'APPLIED')`,
		string(org), string(currency), now).Scan(&u.Last24h, &u.Last7d, &u.Last30d, &u.LastHourCount)
	return u, err
}

// commerceUsage mirrors countsAgainstMandate (checkouts.go), org-scoped
// instead of mandate-scoped: redeemed/paid, or authorized with a grant
// not yet expired. It additionally counts a checkout still
// awaiting_customer_approval: that checkout already passed this
// organisation's policy (the gate runs before the mandate is ever
// evaluated) and is only waiting on the customer, so it must already
// count here - otherwise several such checkouts could each pass
// org-policy evaluation independently without ever seeing each
// other's pending amount. countsAgainstMandate itself stays
// unchanged, since a mandate's own budget isn't spent until approved.
func commerceUsage(ctx context.Context, tx pgx.Tx, org domain.OrgID, currency domain.Currency, now time.Time) (policy.Usage, error) {
	var u policy.Usage
	// countsAgainstMandate (checkouts.go) hardcodes $2 as "now" - $2
	// must stay now here too, so currency moves to $3.
	err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(sum(c.total_minor) FILTER (WHERE c.created_at > $2::timestamptz - interval '24 hours'), 0),
			COALESCE(sum(c.total_minor) FILTER (WHERE c.created_at > $2::timestamptz - interval '7 days'), 0),
			COALESCE(sum(c.total_minor) FILTER (WHERE c.created_at > $2::timestamptz - interval '30 days'), 0),
			count(*) FILTER (WHERE c.created_at > $2::timestamptz - interval '1 hour')
		FROM commerce_checkouts c LEFT JOIN commerce_authorizations a ON a.checkout_id = c.id
		WHERE c.org_id = $1 AND c.currency = $3 AND (`+countsAgainstMandate+` OR c.status = 'awaiting_customer_approval')`,
		string(org), now, string(currency)).Scan(&u.Last24h, &u.Last7d, &u.Last30d, &u.LastHourCount)
	return u, err
}

// EvaluateAndReserve locks the organisation for the duration of the
// evaluation - see policy.Store's doc comment for exactly what
// guarantee that does, and does not, give.
func (s *PolicyStore) EvaluateAndReserve(ctx context.Context, org domain.OrgID, in policy.EvaluationInput) (policy.Decision, error) {
	in.OrgID = org
	now := time.Now().UTC()
	var decision policy.Decision
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOrg(ctx, tx, org); err != nil {
			return err
		}
		version, err := currentPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		usage, err := rollingUsage(ctx, tx, org, in.Currency, now)
		if err != nil {
			return err
		}
		decision = policy.Evaluate(version, in, usage)
		return nil
	})
	return decision, err
}
