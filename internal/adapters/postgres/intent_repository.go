package postgres

import (
	"context"
	"fmt"

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

func (r *IntentRepository) Save(ctx context.Context, intent domain.Intent) (domain.Intent, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO intents (key, payee, minor_units, currency, reference, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (key) DO NOTHING`,
		string(intent.Key), intent.Payee, intent.Amount.MinorUnits(), string(intent.Amount.Currency()), intent.Reference, intent.Reason,
	)
	if err != nil {
		return domain.Intent{}, fmt.Errorf("saving intent %s: %w", intent.Key, err)
	}
	return r.getByKey(ctx, intent.Key)
}

func (r *IntentRepository) getByKey(ctx context.Context, key domain.IntentKey) (domain.Intent, error) {
	var payee, currency, reference, reason string
	var minorUnits int64

	err := r.pool.QueryRow(ctx, `
		SELECT payee, minor_units, currency, reference, reason
		FROM intents WHERE key = $1`, string(key),
	).Scan(&payee, &minorUnits, &currency, &reference, &reason)
	if err != nil {
		return domain.Intent{}, fmt.Errorf("loading intent %s: %w", key, err)
	}

	amount, err := domain.NewMoney(minorUnits, domain.Currency(currency))
	if err != nil {
		return domain.Intent{}, fmt.Errorf("stored intent %s has invalid money: %w", key, err)
	}

	return domain.Intent{
		Payee:     payee,
		Amount:    amount,
		Reference: reference,
		Reason:    reason,
		Key:       key,
	}, nil
}
