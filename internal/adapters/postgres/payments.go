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

var _ app.PaymentReader = (*PaymentReader)(nil)

// PaymentReader reads the intents read model for the dashboard.
type PaymentReader struct {
	pool *pgxpool.Pool
}

func NewPaymentReader(pool *pgxpool.Pool) *PaymentReader { return &PaymentReader{pool: pool} }

const paymentSelect = `
	SELECT i.key, i.org_id, COALESCE(i.state, 'RECEIVED'), i.payee, COALESCE(b.name, ''),
	       i.minor_units, i.currency, i.reference, i.reason,
	       COALESCE(i.agent_id, ''), COALESCE(ag.name, ''),
	       COALESCE(i.transfer_id, ''), COALESCE(i.failure_reason, ''),
	       i.created_at, i.state_changed_at,
	       (SELECT count(*) FROM attempts a WHERE a.org_id = i.org_id AND a.intent_key = i.key AND a.kind = 'agent_submission'),
	       (SELECT count(*) FROM attempts a WHERE a.org_id = i.org_id AND a.intent_key = i.key AND a.kind = 'rail_call')
	FROM intents i
	LEFT JOIN agents ag ON ag.id = i.agent_id
	LEFT JOIN beneficiaries b ON b.org_id = i.org_id AND b.rail_id = i.payee`

func scanPayment(row pgx.Row) (app.Payment, error) {
	var p app.Payment
	var key, org, currency, agent string
	var minor int64
	err := row.Scan(&key, &org, &p.State, &p.Payee, &p.PayeeName, &minor, &currency, &p.Reference, &p.Reason,
		&agent, &p.AgentName, &p.TransferID, &p.FailureReason, &p.CreatedAt, &p.StateChangedAt, &p.Submissions, &p.RailCalls)
	if err != nil {
		return app.Payment{}, err
	}
	amount, err := domain.NewMoney(minor, domain.Currency(currency))
	if err != nil {
		return app.Payment{}, fmt.Errorf("stored intent %s has invalid money: %w", key, err)
	}
	p.Key, p.OrgID, p.AgentID, p.Amount = domain.IntentKey(key), domain.OrgID(org), domain.AgentID(agent), amount
	return p, nil
}

func (r *PaymentReader) GetPayment(ctx context.Context, org domain.OrgID, key domain.IntentKey) (app.Payment, error) {
	p, err := scanPayment(r.pool.QueryRow(ctx, paymentSelect+` WHERE i.org_id = $1 AND i.key = $2`, string(org), string(key)))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Payment{}, app.ErrNotFound
	}
	return p, err
}

func (r *PaymentReader) ListPayments(ctx context.Context, org domain.OrgID, f app.PaymentFilter) (app.PaymentPage, error) {
	w := &where{}
	w.add("i.org_id = ?", string(org))
	if len(f.States) > 0 {
		w.add("COALESCE(i.state, 'RECEIVED') = ANY(?)", f.States)
	}
	if f.AgentID != "" {
		w.add("i.agent_id = ?", string(f.AgentID))
	}
	if f.Payee != "" {
		w.add("i.payee = ?", f.Payee)
	}
	if f.Currency != "" {
		w.add("i.currency = ?", string(f.Currency))
	}
	if f.Reference != "" {
		w.add("i.reference = ?", f.Reference)
	}
	if !f.From.IsZero() {
		w.add("i.created_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		w.add("i.created_at < ?", f.To)
	}
	if f.Cursor != "" {
		t, key, err := decodeCursor(f.Cursor)
		if err != nil {
			return app.PaymentPage{}, &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(i.created_at, i.key) < (?, ?)", t, key)
	}
	limit := w.next(f.Limit + 1)

	rows, err := r.pool.Query(ctx, paymentSelect+` WHERE `+w.sql()+` ORDER BY i.created_at DESC, i.key DESC LIMIT `+limit, w.args...)
	if err != nil {
		return app.PaymentPage{}, fmt.Errorf("listing payments: %w", err)
	}
	defer rows.Close()
	var page app.PaymentPage
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return app.PaymentPage{}, err
		}
		page.Payments = append(page.Payments, p)
	}
	if err := rows.Err(); err != nil {
		return app.PaymentPage{}, err
	}
	if len(page.Payments) > f.Limit {
		page.Payments = page.Payments[:f.Limit]
		last := page.Payments[f.Limit-1]
		page.NextCursor = encodeCursor(last.CreatedAt, string(last.Key))
	}
	return page, nil
}
