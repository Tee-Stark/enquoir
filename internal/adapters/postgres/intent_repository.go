package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.IntentRepository = (*IntentRepository)(nil)

// IntentRepository persists Intents in Postgres, keyed by their
// IntentKey so a resubmitted intent is recognised, not duplicated.
type IntentRepository struct {
	pool *pgxpool.Pool
}

func NewIntentRepository(pool *pgxpool.Pool) *IntentRepository {
	return &IntentRepository{pool: pool}
}

// Save inserts intent if its key is new and returns the row as stored,
// in one round trip; only when the key already exists does it read the
// existing row.
func (r *IntentRepository) Save(ctx context.Context, intent domain.Intent) (domain.Intent, bool, error) {
	var agent *string
	if intent.Agent != "" {
		a := string(intent.Agent)
		agent = &a
	}
	stored, err := scanIntent(r.pool.QueryRow(ctx, `
		INSERT INTO intents (key, org_id, treasury_id, agent_id, payee, minor_units, currency, reference, reason, policy_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (key) DO NOTHING
		RETURNING `+intentColumns,
		string(intent.Key), string(intent.OrgID), string(intent.TreasuryID), agent, intent.Payee, intent.Amount.MinorUnits(), string(intent.Amount.Currency()), intent.Reference, intent.Reason, intent.PolicyVersion,
	), intent.OrgID, intent.Key)
	if errors.Is(err, pgx.ErrNoRows) {
		stored, err = r.getByKey(ctx, intent.OrgID, intent.Key)
		return stored, false, err
	}
	if err != nil {
		return domain.Intent{}, false, fmt.Errorf("saving intent %s: %w", intent.Key, err)
	}
	return stored, true, nil
}

const intentColumns = `payee, minor_units, currency, reference, reason, agent_id, treasury_id, policy_version`

func scanIntent(row pgx.Row, org domain.OrgID, key domain.IntentKey) (domain.Intent, error) {
	var payee, currency, reference, reason, treasury string
	var agent *string
	var minorUnits int64
	var policyVersion *int
	if err := row.Scan(&payee, &minorUnits, &currency, &reference, &reason, &agent, &treasury, &policyVersion); err != nil {
		return domain.Intent{}, err
	}
	amount, err := domain.NewMoney(minorUnits, domain.Currency(currency))
	if err != nil {
		return domain.Intent{}, fmt.Errorf("stored intent %s has invalid money: %w", key, err)
	}
	var agentID domain.AgentID
	if agent != nil {
		agentID = domain.AgentID(*agent)
	}
	return domain.Intent{Agent: agentID, OrgID: org, TreasuryID: domain.TreasuryID(treasury), Payee: payee, Amount: amount, Reference: reference, Reason: reason, Key: key, PolicyVersion: policyVersion}, nil
}

// Tenants are isolated: getByKey scopes by org even though key alone
// cannot belong to another organisation, as a second, independent
// guard against ever returning a cross-tenant row.
func (r *IntentRepository) getByKey(ctx context.Context, org domain.OrgID, key domain.IntentKey) (domain.Intent, error) {
	stored, err := scanIntent(r.pool.QueryRow(ctx, `SELECT `+intentColumns+` FROM intents WHERE org_id = $1 AND key = $2`, string(org), string(key)), org, key)
	if err != nil {
		return domain.Intent{}, fmt.Errorf("loading intent %s: %w", key, err)
	}
	return stored, nil
}
