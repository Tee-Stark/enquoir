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

// checkoutSelect reads a checkout with its authorization, if any.
const checkoutSelect = `
	SELECT c.id, c.snapshot, c.status, c.denials, c.created_at, c.decided_at, c.approval_expires_at, c.canceled_at,
	       a.id, a.grant_hash, a.grant_issued_at, a.created_at, a.expires_at, a.redeemed_at, a.revoked_at, a.execution_id
	FROM commerce_checkouts c
	LEFT JOIN commerce_authorizations a ON a.checkout_id = c.id`

func scanCheckout(row pgx.Row) (commerce.Checkout, error) {
	var c commerce.Checkout
	var id, snap, status string
	var denials []byte
	var authID, grantHash, execID *string
	var grantIssued, authCreated, authExpires, redeemed, revoked *time.Time
	err := row.Scan(&id, &snap, &status, &denials, &c.CreatedAt, &c.DecidedAt, &c.ApprovalExpiresAt, &c.CanceledAt,
		&authID, &grantHash, &grantIssued, &authCreated, &authExpires, &redeemed, &revoked, &execID)
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
	if authID != nil {
		a := commerce.NewAuthorization(commerce.AuthorizationID(*authID), c, *authCreated)
		a.ExpiresAt, a.GrantIssuedAt, a.RedeemedAt, a.RevokedAt = *authExpires, grantIssued, redeemed, revoked
		if grantHash != nil {
			a.GrantHash = *grantHash
		}
		if execID != nil {
			a.ExecutionID = commerce.ExecutionID(*execID)
		}
		c.Authorization = &a
	}
	return c, nil
}

func insertAuthorization(ctx context.Context, tx pgx.Tx, a commerce.Authorization) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO commerce_authorizations (id, checkout_id, mandate_id, merchant_id, org_id, external_checkout_id, checkout_hash,
			amount_minor, currency, funding_method_id, grant_hash, grant_issued_at, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		string(a.ID), string(a.CheckoutID), string(a.MandateID), string(a.MerchantID), string(a.OrgID), string(a.ExternalCheckoutID),
		a.CheckoutHash, a.AmountMinor, string(a.Currency), string(a.FundingMethodID), nullable(a.GrantHash), a.GrantIssuedAt,
		a.CreatedAt, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("storing authorization: %w", err)
	}
	return nil
}

// countsAgainstMandate is commerce.Checkout.CountsAt in SQL, for c (a
// checkout) and a (its authorization), at $2.
const countsAgainstMandate = `(c.status IN ('redeemed', 'paid') OR (c.status = 'authorized' AND a.expires_at > $2))`

// effectiveCheckoutStatus is commerce.Checkout.StatusAt in SQL, at the
// parameter p.
func effectiveCheckoutStatus(p string) string {
	return `(CASE WHEN c.status = 'awaiting_customer_approval' AND c.approval_expires_at <= ` + p + ` THEN 'expired'
		WHEN c.status = 'authorized' AND a.expires_at <= ` + p + ` THEN 'expired' ELSE c.status END)`
}

// lockMandateRow locks a mandate of scope s for the rest of tx and reads
// it, without computing its usage or its effective status - every
// writer that only needs the lock itself (cancel, grant issue/redeem,
// execution reporting, via lockCheckoutsMandate) would otherwise pay for
// mandateSelect's join and CASE expression and then discard both unread.
func lockMandateRow(ctx context.Context, tx pgx.Tx, s commerceapp.Scope, id commerce.MandateID) (commerceapp.MandateRow, error) {
	var locked bool
	err := tx.QueryRow(ctx, `SELECT true FROM commerce_mandates WHERE org_id = $1 AND merchant_id = $2 AND id = $3 FOR UPDATE`,
		string(s.OrgID), string(s.MerchantID), string(id)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerceapp.MandateRow{}, app.ErrNotFound
	}
	if err != nil {
		return commerceapp.MandateRow{}, err
	}
	return scanMandateRow(tx.QueryRow(ctx, mandateRowSelect+` WHERE m.id = $1`, string(id)))
}

// mandateUsage computes what counts against a mandate's budget and
// purchase count: redeemed checkouts, and authorized ones whose grant
// has not expired (commerce.Checkout.CountsAt). Only checkout submission
// and approval-decision evaluate a budget/count limit, so only they call
// this - via lockMandate, which locks the mandate first.
func mandateUsage(ctx context.Context, tx pgx.Tx, id commerce.MandateID, now time.Time) (commerce.MandateUsage, error) {
	var u commerce.MandateUsage
	err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(c.total_minor), 0), COALESCE(sum(c.total_minor) FILTER (WHERE c.status = 'paid'), 0)
		FROM commerce_checkouts c LEFT JOIN commerce_authorizations a ON a.checkout_id = c.id
		WHERE c.mandate_id = $1 AND `+countsAgainstMandate, string(id), now).Scan(&u.Purchases, &u.SpentMinor, &u.PaidMinor)
	return u, err
}

// lockMandate locks a mandate of scope s for the rest of tx and reads it
// with the statuses of what it names, and what it has authorized.
func lockMandate(ctx context.Context, tx pgx.Tx, s commerceapp.Scope, id commerce.MandateID, now time.Time) (commerceapp.MandateRow, commerce.MandateUsage, error) {
	row, err := lockMandateRow(ctx, tx, s, id)
	if err != nil {
		return commerceapp.MandateRow{}, commerce.MandateUsage{}, err
	}
	u, err := mandateUsage(ctx, tx, id, now)
	if err != nil {
		return commerceapp.MandateRow{}, commerce.MandateUsage{}, err
	}
	return row, u, nil
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
	return scanCheckout(q.QueryRow(ctx, checkoutSelect+`
		WHERE c.org_id = $1 AND c.merchant_id = $2 AND c.external_id = $3`, string(sc.OrgID), string(sc.MerchantID), string(ext)))
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
		if created && o.Authorization != nil {
			if err := insertAuthorization(ctx, tx, *o.Authorization); err != nil {
				return err
			}
		}
		out, err = s.byExternalID(ctx, tx, sc, snap.ExternalID)
		return err
	})
	if err != nil {
		return commerce.Checkout{}, false, err
	}
	return out, created, nil
}

func (s *CommerceCheckoutStore) GetCheckout(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID) (commerce.Checkout, error) {
	return scanCheckout(s.pool.QueryRow(ctx, checkoutSelect+`
		WHERE c.org_id = $1 AND c.merchant_id = $2 AND c.id = $3`, string(sc.OrgID), string(sc.MerchantID), string(id)))
}

func (s *CommerceCheckoutStore) CheckoutForApproval(ctx context.Context, id commerce.CheckoutID) (commerce.Checkout, error) {
	return scanCheckout(s.pool.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, string(id)))
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
		if o.Authorization != nil {
			if err := insertAuthorization(ctx, tx, *o.Authorization); err != nil {
				return err
			}
		}
		out, err = scanCheckout(tx.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, string(id)))
		return err
	})
	return out, err
}

// lockCheckoutsMandate finds s's checkout id and locks its mandate for
// the rest of tx: the lock every write to a mandate's checkouts and
// grants takes first.
func lockCheckoutsMandate(ctx context.Context, tx pgx.Tx, sc commerceapp.Scope, id commerce.CheckoutID) (commerceapp.MandateRow, error) {
	var mandate string
	err := tx.QueryRow(ctx, `SELECT mandate_id FROM commerce_checkouts WHERE org_id = $1 AND merchant_id = $2 AND id = $3`,
		string(sc.OrgID), string(sc.MerchantID), string(id)).Scan(&mandate)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerceapp.MandateRow{}, app.ErrNotFound
	}
	if err != nil {
		return commerceapp.MandateRow{}, err
	}
	return lockMandateRow(ctx, tx, sc, commerce.MandateID(mandate))
}

func (s *CommerceCheckoutStore) CancelCheckout(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID, at time.Time) (commerce.Checkout, bool, error) {
	var out commerce.Checkout
	changed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockCheckoutsMandate(ctx, tx, sc, id); err != nil {
			return err
		}
		c, err := scanCheckout(tx.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, string(id)))
		if err != nil {
			return err
		}
		switch c.StatusAt(at) {
		case commerce.CheckoutCanceled:
			out = c
			return nil
		case commerce.CheckoutAuthorized, commerce.CheckoutAwaitingApproval:
		default:
			return commerce.ErrCheckoutNotCancelable
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce_checkouts SET status = 'canceled', canceled_at = $2 WHERE id = $1`, string(id), at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce_authorizations SET revoked_at = $2 WHERE checkout_id = $1 AND redeemed_at IS NULL AND revoked_at IS NULL`, string(id), at); err != nil {
			return err
		}
		changed = true
		out, err = scanCheckout(tx.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, string(id)))
		return err
	})
	if err != nil {
		return commerce.Checkout{}, false, err
	}
	return out, changed, nil
}

func (s *CommerceCheckoutStore) IssueGrant(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID, grantHash string, at time.Time) (commerce.Checkout, error) {
	var out commerce.Checkout
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockCheckoutsMandate(ctx, tx, sc, id); err != nil {
			return err
		}
		c, err := scanCheckout(tx.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, string(id)))
		if err != nil {
			return err
		}
		if c.Authorization == nil {
			return commerceapp.ErrGrantNotIssuable
		}
		switch c.Authorization.StatusAt(at) {
		case commerce.AuthorizationRedeemed:
			return commerce.ErrGrantRedeemed
		case commerce.AuthorizationActive:
		default:
			return commerceapp.ErrGrantNotIssuable
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce_authorizations SET grant_hash = $2, grant_issued_at = $3 WHERE id = $1`,
			string(c.Authorization.ID), grantHash, at); err != nil {
			return err
		}
		out, err = scanCheckout(tx.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, string(id)))
		return err
	})
	return out, err
}

var executionSelect = `
	SELECT e.id, e.authorization_id, e.checkout_id, e.external_checkout_id, e.checkout_hash, e.mandate_id, e.merchant_id, e.org_id,
	       e.amount_minor, e.currency, e.status, e.created_at, COALESCE(e.provider_payment_reference, ''), COALESCE(e.failure_reason, ''),
	       e.updated_at, e.report_deadline, e.finished_at, ` + fundingColumnsAs("f") + `
	FROM commerce_executions e
	JOIN commerce_funding_methods f ON f.id = e.funding_method_id`

func scanExecution(row pgx.Row) (commerce.Execution, error) {
	var e commerce.Execution
	var id, auth, checkout, ext, mandate, merchant, org, currency, status string
	var f commerce.FundingMethod
	var fid, fmerchant, forg, fcustomer, ref, pref, provider, fstatus string
	err := row.Scan(&id, &auth, &checkout, &ext, &e.CheckoutHash, &mandate, &merchant, &org, &e.AmountMinor, &currency, &status, &e.CreatedAt,
		&e.ProviderPaymentReference, &e.FailureReason, &e.UpdatedAt, &e.ReportDeadline, &e.FinishedAt,
		&fid, &fmerchant, &forg, &fcustomer, &ref, &pref, &provider, &f.Display.Brand, &f.Display.LastFour, &fstatus, &f.CreatedAt, &f.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Execution{}, app.ErrNotFound
	}
	if err != nil {
		return commerce.Execution{}, err
	}
	f.ID, f.MerchantID, f.OrgID, f.CustomerID = commerce.FundingMethodID(fid), commerce.MerchantID(fmerchant), domain.OrgID(forg), commerce.CustomerID(fcustomer)
	f.Reference, f.ProviderReference, f.Provider, f.Status = commerce.FundingReference(ref), commerce.ProviderReference(pref), commerce.Provider(provider), commerce.Status(fstatus)
	e.ID, e.AuthorizationID, e.CheckoutID, e.ExternalCheckoutID = commerce.ExecutionID(id), commerce.AuthorizationID(auth), commerce.CheckoutID(checkout), commerce.ExternalID(ext)
	e.MandateID, e.MerchantID, e.OrgID = commerce.MandateID(mandate), commerce.MerchantID(merchant), domain.OrgID(org)
	e.Currency, e.Status, e.FundingMethod = domain.Currency(currency), commerce.ExecutionStatus(status), f
	return e, nil
}

// RedeemGrant finds the grant, locks its mandate, then re-reads the
// authorization under that lock: a concurrent redemption that won has
// committed by then, and is returned instead of making a second.
func (s *CommerceCheckoutStore) RedeemGrant(ctx context.Context, sc commerceapp.Scope, grantHash string, at time.Time, redeem commerceapp.GrantRedeemer) (commerce.Execution, bool, error) {
	var out commerce.Execution
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var checkout string
		err := tx.QueryRow(ctx, `SELECT checkout_id FROM commerce_authorizations WHERE org_id = $1 AND merchant_id = $2 AND grant_hash = $3`,
			string(sc.OrgID), string(sc.MerchantID), grantHash).Scan(&checkout)
		if errors.Is(err, pgx.ErrNoRows) {
			return commerceapp.ErrGrantInvalid
		}
		if err != nil {
			return err
		}
		m, err := lockCheckoutsMandate(ctx, tx, sc, commerce.CheckoutID(checkout))
		if err != nil {
			return err
		}
		c, err := scanCheckout(tx.QueryRow(ctx, checkoutSelect+` WHERE c.id = $1`, checkout))
		if err != nil {
			return err
		}
		a := c.Authorization
		if a == nil || a.GrantHash != grantHash {
			return commerceapp.ErrGrantInvalid // reissued meanwhile
		}
		if a.RedeemedAt != nil {
			out, err = scanExecution(tx.QueryRow(ctx, executionSelect+` WHERE e.id = $1`, string(a.ExecutionID)))
			return err
		}
		f, err := scanFundingMethod(tx.QueryRow(ctx, `SELECT `+fundingColumns+` FROM commerce_funding_methods WHERE id = $1`, string(a.FundingMethodID)))
		if err != nil {
			return err
		}
		e, err := redeem(*a, m, f)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO commerce_executions (id, authorization_id, checkout_id, external_checkout_id, checkout_hash, mandate_id, merchant_id, org_id,
				amount_minor, currency, funding_method_id, status, created_at, updated_at, report_deadline)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			string(e.ID), string(e.AuthorizationID), string(e.CheckoutID), string(e.ExternalCheckoutID), e.CheckoutHash, string(e.MandateID),
			string(e.MerchantID), string(e.OrgID), e.AmountMinor, string(e.Currency), string(e.FundingMethod.ID), string(e.Status), e.CreatedAt,
			e.UpdatedAt, e.ReportDeadline); err != nil {
			return fmt.Errorf("storing execution: %w", err)
		}
		if err := appendExecutionEvent(ctx, tx, e.ID, commerce.ExecutionEvent{Kind: commerce.EventRedeemed, StatusAfter: e.Status,
			AmountMinor: e.AmountMinor, Currency: e.Currency, At: at}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce_authorizations SET redeemed_at = $2, execution_id = $3 WHERE id = $1`, string(a.ID), at, string(e.ID)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce_checkouts SET status = 'redeemed' WHERE id = $1`, checkout); err != nil {
			return err
		}
		out, created = e, true
		return nil
	})
	if err != nil {
		return commerce.Execution{}, false, err
	}
	return out, created, nil
}

func (s *CommerceCheckoutStore) GetExecution(ctx context.Context, sc commerceapp.Scope, id commerce.ExecutionID) (commerce.Execution, error) {
	return scanExecution(s.pool.QueryRow(ctx, executionSelect+` WHERE e.org_id = $1 AND e.merchant_id = $2 AND e.id = $3`,
		string(sc.OrgID), string(sc.MerchantID), string(id)))
}
