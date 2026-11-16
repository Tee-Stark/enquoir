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

var _ app.BeneficiaryStore = (*BeneficiaryStore)(nil)

// BeneficiaryStore records which organisation registered which payee.
type BeneficiaryStore struct {
	pool *pgxpool.Pool
}

func NewBeneficiaryStore(pool *pgxpool.Pool) *BeneficiaryStore { return &BeneficiaryStore{pool: pool} }

func (s *BeneficiaryStore) Save(ctx context.Context, b app.RegisteredBeneficiary) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO beneficiaries (key, org_id, treasury_id, rail_id, name, account_last4, routing_number, account_category, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (key) DO NOTHING`,
		string(b.Key), string(b.OrgID), string(b.TreasuryID), b.RailID, b.Name, b.AccountLast4, b.RoutingNumber, string(b.Category), b.CreatedAt)
	if isUniqueViolation(err, "beneficiaries_rail_id_key") {
		return fmt.Errorf("rail beneficiary %s is already owned by another organisation", b.RailID)
	}
	if err != nil {
		return fmt.Errorf("saving beneficiary: %w", err)
	}
	return nil
}

func (s *BeneficiaryStore) Owns(ctx context.Context, org domain.OrgID, treasury domain.TreasuryID, railID string) (bool, error) {
	var owns bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM beneficiaries WHERE org_id = $1 AND treasury_id = $2 AND rail_id = $3)`,
		string(org), string(treasury), railID).Scan(&owns)
	if err != nil {
		return false, fmt.Errorf("checking beneficiary ownership: %w", err)
	}
	return owns, nil
}

func (s *BeneficiaryStore) List(ctx context.Context, org domain.OrgID) ([]app.RegisteredBeneficiary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, rail_id, name, account_last4, routing_number, account_category, created_at, treasury_id
		FROM beneficiaries WHERE org_id = $1 ORDER BY created_at DESC, key DESC`, string(org))
	if err != nil {
		return nil, fmt.Errorf("listing beneficiaries: %w", err)
	}
	defer rows.Close()
	var out []app.RegisteredBeneficiary
	for rows.Next() {
		b := app.RegisteredBeneficiary{OrgID: org}
		var key, category, treasury string
		if err := rows.Scan(&key, &b.RailID, &b.Name, &b.AccountLast4, &b.RoutingNumber, &category, &b.CreatedAt, &treasury); err != nil {
			return nil, err
		}
		b.Key, b.Category, b.TreasuryID = domain.BeneficiaryKey(key), domain.AccountCategory(category), domain.TreasuryID(treasury)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *BeneficiaryStore) ByKey(ctx context.Context, org domain.OrgID, treasury domain.TreasuryID, key domain.BeneficiaryKey) (app.RegisteredBeneficiary, error) {
	b := app.RegisteredBeneficiary{OrgID: org, TreasuryID: treasury, Key: key}
	var category string
	err := s.pool.QueryRow(ctx, `SELECT rail_id, name, account_last4, routing_number, account_category, created_at
		FROM beneficiaries WHERE org_id = $1 AND treasury_id = $2 AND key = $3`, string(org), string(treasury), string(key)).
		Scan(&b.RailID, &b.Name, &b.AccountLast4, &b.RoutingNumber, &category, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.RegisteredBeneficiary{}, app.ErrNotFound
	}
	if err != nil {
		return app.RegisteredBeneficiary{}, fmt.Errorf("finding beneficiary: %w", err)
	}
	b.Category = domain.AccountCategory(category)
	return b, nil
}
