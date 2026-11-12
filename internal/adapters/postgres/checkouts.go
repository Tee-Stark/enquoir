package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

var _ commerceapp.CheckoutStore = (*CommerceCheckoutStore)(nil)

// CommerceCheckoutStore persists checkouts and decides them under a lock
// on their mandate.
type CommerceCheckoutStore struct{ pool *pgxpool.Pool }

func NewCommerceCheckoutStore(pool *pgxpool.Pool) *CommerceCheckoutStore {
	return &CommerceCheckoutStore{pool: pool}
}

const checkoutColumns = `id, snapshot, status, denials, created_at, decided_at, approval_expires_at, canceled_at`

func scanCheckout(row pgx.Row) (commerce.Checkout, error) {
	var c commerce.Checkout
	var id, snap, status string
	var denials []byte
	err := row.Scan(&id, &snap, &status, &denials, &c.CreatedAt, &c.DecidedAt, &c.ApprovalExpiresAt, &c.CanceledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Checkout{}, app.ErrNotFound
	}
	if err != nil {
		return commerce.Checkout{}, err
	}
	if c.Snapshot, err = commerce.ParseCheckoutSnapshot([]byte(snap)); err != nil {
		return commerce.Checkout{}, err
	}
	if err := json.Unmarshal(denials, &c.Denials); err != nil {
		return commerce.Checkout{}, fmt.Errorf("reading denials: %w", err)
	}
	c.ID, c.Status = commerce.CheckoutID(id), commerce.CheckoutStatus(status)
	return c, nil
}

// lockMandate locks a mandate of scope s for the rest of tx and reads it
// with the statuses of what it names, and what it has authorized.
func lockMandate(ctx context.Context, tx pgx.Tx, s commerceapp.Scope, id commerce.MandateID, now time.Time) (commerceapp.MandateRow, commerce.MandateUsage, error) {
	var locked bool
	err := tx.QueryRow(ctx, `SELECT true FROM commerce_mandates WHERE org_id = $1 AND merchant_id = $2 AND id = $3 FOR UPDATE`,
		string(s.OrgID), string(s.MerchantID), string(id)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerceapp.MandateRow{}, commerce.MandateUsage{}, app.ErrNotFound
	}
	if err != nil {
		return commerceapp.MandateRow{}, commerce.MandateUsage{}, err
	}
	row, _, err := scanMandate(tx.QueryRow(ctx, mandateSelect+` WHERE id = $2`, now, string(id)))
	if err != nil {
		return commerceapp.MandateRow{}, commerce.MandateUsage{}, err
	}
	var u commerce.MandateUsage
	err = tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(total_minor), 0) FROM commerce_checkouts
		WHERE mandate_id = $1 AND status = 'authorized'`, string(id)).Scan(&u.Purchases, &u.SpentMinor)
	return row, u, err
}

func denialsJSON(d []commerce.Denial) []byte {
	if d == nil {
		d = []commerce.Denial{}
	}
	b, _ := json.Marshal(d)
	return b
}

func decidedAt(status commerce.CheckoutStatus, at time.Time) *time.Time {
	if status == commerce.CheckoutAwaitingApproval {
		return nil
	}
	return &at
}

func (s *CommerceCheckoutStore) byExternalID(ctx context.Context, q pgx.Tx, sc commerceapp.Scope, ext commerce.ExternalID) (commerce.Checkout, error) {
	return scanCheckout(q.QueryRow(ctx, `SELECT `+checkoutColumns+` FROM commerce_checkouts
		WHERE org_id = $1 AND merchant_id = $2 AND external_id = $3`, string(sc.OrgID), string(sc.MerchantID), string(ext)))
}

func (s *CommerceCheckoutStore) SubmitCheckout(ctx context.Context, sc commerceapp.Scope, c commerce.Checkout, decide commerceapp.CheckoutDecider) (commerce.Checkout, bool, error) {
	var out commerce.Checkout
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if have, err := s.byExternalID(ctx, tx, sc, c.Snapshot.ExternalID); err == nil {
			out = have
			return nil
		} else if !errors.Is(err, app.ErrNotFound) {
			return err
		}
		row, usage, err := lockMandate(ctx, tx, sc, c.Snapshot.MandateID, c.CreatedAt)
		if err != nil {
			return err
		}
		o := decide(row, usage)
		snap := c.Snapshot
		tag, err := tx.Exec(ctx, `
			INSERT INTO commerce_checkouts (id, merchant_id, org_id, mandate_id, external_id, customer_id, agent_id, funding_method_id,
				currency, total_minor, snapshot, snapshot_hash, status, denials, created_at, decided_at, approval_expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (merchant_id, external_id) DO NOTHING`,
			string(c.ID), string(snap.MerchantID), string(snap.OrgID), string(snap.MandateID), string(snap.ExternalID),
			string(snap.CustomerID), string(snap.AgentID), string(snap.FundingMethodID), string(snap.Currency), snap.TotalMinor,
			string(snap.Canonical()), snap.Hash(), string(o.Status), denialsJSON(o.Denials), c.CreatedAt,
			decidedAt(o.Status, c.CreatedAt), o.ApprovalExpiresAt)
		if err != nil {
			return fmt.Errorf("storing checkout: %w", err)
		}
		created = tag.RowsAffected() == 1
		out, err = s.byExternalID(ctx, tx, sc, snap.ExternalID)
		return err
	})
	if err != nil {
		return commerce.Checkout{}, false, err
	}
	return out, created, nil
}

func (s *CommerceCheckoutStore) GetCheckout(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID) (commerce.Checkout, error) {
	return scanCheckout(s.pool.QueryRow(ctx, `SELECT `+checkoutColumns+` FROM commerce_checkouts
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3`, string(sc.OrgID), string(sc.MerchantID), string(id)))
}

func (s *CommerceCheckoutStore) CheckoutForApproval(ctx context.Context, id commerce.CheckoutID) (commerce.Checkout, error) {
	return scanCheckout(s.pool.QueryRow(ctx, `SELECT `+checkoutColumns+` FROM commerce_checkouts WHERE id = $1`, string(id)))
}

// DecideCheckout locks the mandate, spends the approval link, decides,
// and records the decision only if the checkout is still awaiting
// approval: all in one transaction.
func (s *CommerceCheckoutStore) DecideCheckout(ctx context.Context, tokenHash string, id commerce.CheckoutID, at time.Time, decide commerceapp.CheckoutDecider) (commerce.Checkout, error) {
	var out commerce.Checkout
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var mandate, merchant, org string
		err := tx.QueryRow(ctx, `SELECT mandate_id, merchant_id, org_id FROM commerce_checkouts WHERE id = $1`, string(id)).Scan(&mandate, &merchant, &org)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrTokenInvalid
		}
		if err != nil {
			return err
		}
		sc := commerceapp.Scope{OrgID: domain.OrgID(org), MerchantID: commerce.MerchantID(merchant)}
		row, usage, err := lockMandate(ctx, tx, sc, commerce.MandateID(mandate), at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE one_time_tokens SET used_at = $3
			WHERE purpose = 'checkout_approval' AND subject = $1 AND used_at IS NULL AND expires_at > $3
			  AND EXISTS (SELECT 1 FROM one_time_tokens t WHERE t.token_hash = $2 AND t.purpose = 'checkout_approval'
			              AND t.subject = $1 AND t.used_at IS NULL AND t.expires_at > $3)`, string(id), tokenHash, at)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return app.ErrTokenInvalid
		}
		o := decide(row, usage)
		tag, err = tx.Exec(ctx, `UPDATE commerce_checkouts SET status = $3, denials = $4, decided_at = $2
			WHERE id = $1 AND status = 'awaiting_customer_approval' AND approval_expires_at > $2`,
			string(id), at, string(o.Status), denialsJSON(o.Denials))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return app.ErrTokenInvalid
		}
		out, err = scanCheckout(tx.QueryRow(ctx, `SELECT `+checkoutColumns+` FROM commerce_checkouts WHERE id = $1`, string(id)))
		return err
	})
	return out, err
}

func (s *CommerceCheckoutStore) CancelCheckout(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID, at time.Time) (commerce.Checkout, bool, error) {
	c, err := scanCheckout(s.pool.QueryRow(ctx, `UPDATE commerce_checkouts SET status = 'canceled', canceled_at = $4
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3
		  AND (status = 'authorized' OR (status = 'awaiting_customer_approval' AND approval_expires_at > $4))
		RETURNING `+checkoutColumns, string(sc.OrgID), string(sc.MerchantID), string(id), at))
	if err == nil {
		return c, true, nil
	}
	if !errors.Is(err, app.ErrNotFound) {
		return commerce.Checkout{}, false, err
	}
	c, err = s.GetCheckout(ctx, sc, id)
	if err != nil {
		return commerce.Checkout{}, false, err
	}
	if c.Status == commerce.CheckoutCanceled {
		return c, false, nil
	}
	return commerce.Checkout{}, false, commerce.ErrCheckoutNotCancelable
}
