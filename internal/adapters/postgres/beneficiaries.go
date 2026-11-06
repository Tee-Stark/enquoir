package postgres

import (
	"context"
	"fmt"

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
		INSERT INTO beneficiaries (key, org_id, rail_id, name, account_last4, routing_number, account_category, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (key) DO NOTHING`,
		string(b.Key), string(b.OrgID), b.RailID, b.Name, b.AccountLast4, b.RoutingNumber, string(b.Category), b.CreatedAt)
	if isUniqueViolation(err, "beneficiaries_rail_id_key") {
		return fmt.Errorf("rail beneficiary %s is already owned by another organisation", b.RailID)
	}
	if err != nil {
		return fmt.Errorf("saving beneficiary: %w", err)
	}
	return nil
}

func (s *BeneficiaryStore) Owns(ctx context.Context, org domain.OrgID, railID string) (bool, error) {
	var owns bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM beneficiaries WHERE org_id = $1 AND rail_id = $2)`,
		string(org), railID).Scan(&owns)
	if err != nil {
		return false, fmt.Errorf("checking beneficiary ownership: %w", err)
	}
	return owns, nil
}

func (s *BeneficiaryStore) List(ctx context.Context, org domain.OrgID) ([]app.RegisteredBeneficiary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, rail_id, name, account_last4, routing_number, account_category, created_at
		FROM beneficiaries WHERE org_id = $1 ORDER BY created_at DESC, key DESC`, string(org))
	if err != nil {
		return nil, fmt.Errorf("listing beneficiaries: %w", err)
	}
	defer rows.Close()
	var out []app.RegisteredBeneficiary
	for rows.Next() {
		b := app.RegisteredBeneficiary{OrgID: org}
		var key, category string
		if err := rows.Scan(&key, &b.RailID, &b.Name, &b.AccountLast4, &b.RoutingNumber, &category, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Key, b.Category = domain.BeneficiaryKey(key), domain.AccountCategory(category)
		out = append(out, b)
	}
	return out, rows.Err()
}
