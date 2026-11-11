package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

var _ commerceapp.MandateStore = (*CommerceMandateStore)(nil)

// CommerceMandateStore persists mandate requests, consent records and
// mandates.
type CommerceMandateStore struct{ pool *pgxpool.Pool }

func NewCommerceMandateStore(pool *pgxpool.Pool) *CommerceMandateStore {
	return &CommerceMandateStore{pool: pool}
}

const requestSelect = `
	SELECT r.id, r.external_id, r.terms, r.status, r.created_at, r.expires_at, r.decided_at, COALESCE(m.id, '')
	FROM commerce_mandate_requests r
	LEFT JOIN commerce_mandates m ON m.request_id = r.id`

func scanRequest(row pgx.Row) (commerce.MandateRequest, error) {
	var r commerce.MandateRequest
	var id, ext, terms, status, mandate string
	err := row.Scan(&id, &ext, &terms, &status, &r.CreatedAt, &r.ExpiresAt, &r.DecidedAt, &mandate)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.MandateRequest{}, app.ErrNotFound
	}
	if err != nil {
		return commerce.MandateRequest{}, err
	}
	if r.Terms, err = commerce.ParseMandateTerms([]byte(terms)); err != nil {
		return commerce.MandateRequest{}, err
	}
	r.ID, r.ExternalID, r.Status, r.MandateID = commerce.MandateRequestID(id), commerce.ExternalID(ext), commerce.RequestStatus(status), commerce.MandateID(mandate)
	return r, nil
}

func (s *CommerceMandateStore) CreateRequest(ctx context.Context, r commerce.MandateRequest) (commerce.MandateRequest, bool, error) {
	p := r.Terms.Parties
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO commerce_mandate_requests (id, merchant_id, org_id, external_id, customer_id, agent_id, funding_method_id,
			terms, terms_hash, status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (merchant_id, external_id) DO NOTHING`,
		string(r.ID), string(p.MerchantID), string(p.OrgID), string(r.ExternalID), string(p.CustomerID), string(p.AgentID),
		string(p.FundingMethodID), string(r.Terms.Canonical()), r.Terms.Hash(), string(r.Status), r.CreatedAt, r.ExpiresAt)
	if err != nil {
		return commerce.MandateRequest{}, false, fmt.Errorf("storing mandate request: %w", err)
	}
	stored, err := scanRequest(s.pool.QueryRow(ctx, requestSelect+` WHERE r.org_id = $1 AND r.merchant_id = $2 AND r.external_id = $3`,
		string(p.OrgID), string(p.MerchantID), string(r.ExternalID)))
	return stored, tag.RowsAffected() == 1, err
}

func (s *CommerceMandateStore) GetRequest(ctx context.Context, sc commerceapp.Scope, id commerce.MandateRequestID) (commerce.MandateRequest, error) {
	return scanRequest(s.pool.QueryRow(ctx, requestSelect+` WHERE r.org_id = $1 AND r.merchant_id = $2 AND r.id = $3`,
		string(sc.OrgID), string(sc.MerchantID), string(id)))
}

func (s *CommerceMandateStore) RequestForConsent(ctx context.Context, id commerce.MandateRequestID) (commerce.MandateRequest, error) {
	return scanRequest(s.pool.QueryRow(ctx, requestSelect+` WHERE r.id = $1`, string(id)))
}

// Decide spends the consent link, decides the request, and writes the
// consent record and any mandate, in one transaction: all or nothing.
func (s *CommerceMandateStore) Decide(ctx context.Context, cmd commerceapp.ConsentDecisionCommand) error {
	rec := cmd.Record
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE commerce_mandate_requests SET status = $2, decided_at = $3
			WHERE id = $1 AND status = 'pending' AND expires_at > $3`, string(rec.RequestID), string(rec.Decision), cmd.At)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return app.ErrTokenInvalid
		}
		// Spend this request's link - and any other for it - here, so the
		// link works once and only together with everything else.
		tag, err = tx.Exec(ctx, `
			UPDATE one_time_tokens SET used_at = $3
			WHERE purpose = 'mandate_consent' AND subject = $1 AND used_at IS NULL AND expires_at > $3
			  AND EXISTS (SELECT 1 FROM one_time_tokens t WHERE t.token_hash = $2 AND t.purpose = 'mandate_consent'
			              AND t.subject = $1 AND t.used_at IS NULL AND t.expires_at > $3)`,
			string(rec.RequestID), cmd.TokenHash, cmd.At)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return app.ErrTokenInvalid
		}
		ua := rec.UserAgent
		if len(ua) > 300 {
			ua = ua[:300]
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO commerce_consent_records (id, request_id, org_id, merchant_id, customer_id, decision, snapshot, terms_hash, method, ip, user_agent, at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			string(rec.ID), string(rec.RequestID), string(rec.OrgID), string(rec.MerchantID), string(rec.CustomerID),
			string(rec.Decision), string(rec.Snapshot), rec.TermsHash, rec.Method, nullable(rec.IP), nullable(ua), rec.At); err != nil {
			return fmt.Errorf("writing consent record: %w", err)
		}
		if m := cmd.Mandate; m != nil {
			p := m.Terms.Parties
			if _, err := tx.Exec(ctx, `
				INSERT INTO commerce_mandates (id, request_id, consent_record_id, merchant_id, org_id, customer_id, agent_id,
					funding_method_id, terms, terms_hash, expires_at, approved_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				string(m.ID), string(m.RequestID), string(m.ConsentRecordID), string(p.MerchantID), string(p.OrgID),
				string(p.CustomerID), string(p.AgentID), string(p.FundingMethodID), string(m.Terms.Canonical()), m.Terms.Hash(),
				m.Terms.ExpiresAt, m.ApprovedAt); err != nil {
				return fmt.Errorf("creating mandate: %w", err)
			}
		}
		return nil
	})
}

// mandateSelect reads mandates with the statuses of what they name and,
// as of $1, their effective status (commerce.Mandate.StatusAt, in SQL,
// so lists can filter on it).
const mandateSelect = `
	SELECT * FROM (
		SELECT m.id, m.request_id, m.consent_record_id, m.terms, m.approved_at, m.revoked_at, m.org_id, m.merchant_id,
		       m.customer_id, mi.status = 'active', c.status = 'active', a.status = 'active', f.status = 'active',
		       CASE WHEN m.revoked_at IS NOT NULL THEN 'revoked'
		            WHEN m.expires_at <= $1 THEN 'expired'
		            WHEN mi.status <> 'active' OR c.status <> 'active' OR a.status <> 'active' OR f.status <> 'active' THEN 'suspended'
		            ELSE 'active' END AS effective
		FROM commerce_mandates m
		JOIN merchant_integrations mi ON mi.id = m.merchant_id
		JOIN commerce_customers c ON c.id = m.customer_id
		JOIN commerce_agents a ON a.id = m.agent_id
		JOIN commerce_funding_methods f ON f.id = m.funding_method_id
	) mm`

func scanMandate(row pgx.Row) (commerceapp.MandateRow, string, error) {
	var r commerceapp.MandateRow
	var id, req, consent, terms, org, merchant, customer, effective string
	err := row.Scan(&id, &req, &consent, &terms, &r.ApprovedAt, &r.RevokedAt, &org, &merchant, &customer,
		&r.Deps.IntegrationActive, &r.Deps.CustomerActive, &r.Deps.AgentActive, &r.Deps.FundingMethodActive, &effective)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerceapp.MandateRow{}, "", app.ErrNotFound
	}
	if err != nil {
		return commerceapp.MandateRow{}, "", err
	}
	if r.Terms, err = commerce.ParseMandateTerms([]byte(terms)); err != nil {
		return commerceapp.MandateRow{}, "", err
	}
	r.ID, r.RequestID, r.ConsentRecordID, r.Revoked = commerce.MandateID(id), commerce.MandateRequestID(req), commerce.ConsentRecordID(consent), r.RevokedAt != nil
	return r, effective, nil
}

func (s *CommerceMandateStore) GetMandate(ctx context.Context, sc commerceapp.Scope, id commerce.MandateID) (commerceapp.MandateRow, error) {
	r, _, err := scanMandate(s.pool.QueryRow(ctx, mandateSelect+` WHERE org_id = $2 AND merchant_id = $3 AND id = $4`,
		time.Now().UTC(), string(sc.OrgID), string(sc.MerchantID), string(id)))
	return r, err
}

func (s *CommerceMandateStore) ListMandates(ctx context.Context, sc commerceapp.Scope, f commerceapp.MandateFilter, now time.Time) ([]commerceapp.MandateRow, string, error) {
	w := &where{}
	w.next(now) // $1, the effective-status clock
	w.add("org_id = ?", string(sc.OrgID))
	w.add("merchant_id = ?", string(sc.MerchantID))
	if f.CustomerID != "" {
		w.add("customer_id = ?", string(f.CustomerID))
	}
	if f.Status != "" {
		w.add("effective = ?", string(f.Status))
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(approved_at, id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	rows, err := s.pool.Query(ctx, mandateSelect+` WHERE `+w.sql()+` ORDER BY approved_at DESC, id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing mandates: %w", err)
	}
	defer rows.Close()
	var out []commerceapp.MandateRow
	for rows.Next() {
		r, _, err := scanMandate(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		last := out[f.Limit-1]
		next = encodeCursor(last.ApprovedAt, string(last.ID))
	}
	return out, next, nil
}

func (s *CommerceMandateStore) RevokeMandate(ctx context.Context, sc commerceapp.Scope, id commerce.MandateID, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE commerce_mandates SET revoked_at = $4
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3 AND revoked_at IS NULL`,
		string(sc.OrgID), string(sc.MerchantID), string(id), at)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	_, err = s.GetMandate(ctx, sc, id)
	return false, err
}
