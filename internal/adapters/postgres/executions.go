package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// appendExecutionEvent adds the next entry to an execution's history.
// The caller holds the execution's row lock, so sequences don't collide.
func appendExecutionEvent(ctx context.Context, tx pgx.Tx, id commerce.ExecutionID, ev commerce.ExecutionEvent) error {
	var amount *int64
	if ev.AmountMinor != 0 {
		amount = &ev.AmountMinor
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO commerce_execution_events (execution_id, sequence, kind, source, outcome, status_after, reference, event_id,
			amount_minor, currency, detail, at)
		SELECT $1, COALESCE(max(sequence), 0) + 1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		FROM commerce_execution_events WHERE execution_id = $1`,
		string(id), string(ev.Kind), nullable(string(ev.Source)), nullable(string(ev.Outcome)), string(ev.StatusAfter),
		nullable(ev.Reference), nullable(ev.EventID), amount, nullable(string(ev.Currency)), nullable(ev.Detail), ev.At)
	if err != nil {
		return fmt.Errorf("recording execution history: %w", err)
	}
	return nil
}

// ReportExecution locks the mandate, then the execution, then decides.
// A refused report is recorded and committed before its error returns.
func (s *CommerceCheckoutStore) ReportExecution(ctx context.Context, sc commerceapp.Scope, id commerce.ExecutionID, r commerce.ExecutionReport, at time.Time, apply commerceapp.ExecutionApplier) (commerce.Execution, commerce.ReportResult, error) {
	var out commerce.Execution
	var result commerce.ReportResult
	var refused error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var checkout string
		err := tx.QueryRow(ctx, `SELECT checkout_id FROM commerce_executions WHERE org_id = $1 AND merchant_id = $2 AND id = $3`,
			string(sc.OrgID), string(sc.MerchantID), string(id)).Scan(&checkout)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := lockCheckoutsMandate(ctx, tx, sc, commerce.CheckoutID(checkout)); err != nil {
			return err
		}
		e, err := scanExecution(tx.QueryRow(ctx, executionSelect+` WHERE e.id = $1 FOR UPDATE OF e`, string(id)))
		if err != nil {
			return err
		}
		refuse := func(err error) error {
			refused = err
			return appendExecutionEvent(ctx, tx, id, commerce.EventFor(r, e.Status, "", err, at))
		}
		if r.EventID != "" {
			var seenExec, seenPrint string
			err := tx.QueryRow(ctx, `SELECT execution_id, fingerprint FROM commerce_webhook_events
				WHERE merchant_id = $1 AND provider = $2 AND event_id = $3`, string(sc.MerchantID), string(r.Provider), r.EventID).Scan(&seenExec, &seenPrint)
			switch {
			case err == nil && seenExec == string(id) && seenPrint == r.Fingerprint():
				out, result = e, commerce.ReportDuplicate
				return appendExecutionEvent(ctx, tx, id, commerce.EventFor(r, e.Status, result, nil, at))
			case err == nil:
				return refuse(commerce.ErrEventConflict)
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}
		next, res, err := apply(e)
		if err != nil {
			return refuse(err)
		}
		if r.EventID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO commerce_webhook_events (merchant_id, provider, event_id, execution_id, fingerprint, at)
				VALUES ($1, $2, $3, $4, $5, $6)`, string(sc.MerchantID), string(r.Provider), r.EventID, string(id), r.Fingerprint(), at); err != nil {
				return err
			}
		}
		if err := appendExecutionEvent(ctx, tx, id, commerce.EventFor(r, next.Status, res, nil, at)); err != nil {
			return err
		}
		out, result = next, res
		if res != commerce.ReportApplied {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE commerce_executions SET status = $2, provider_payment_reference = $3, failure_reason = $4,
			updated_at = $5, finished_at = $6 WHERE id = $1`, string(id), string(next.Status), nullable(next.ProviderPaymentReference),
			nullable(next.FailureReason), next.UpdatedAt, next.FinishedAt); err != nil {
			return err
		}
		settled := map[commerce.ExecutionStatus]commerce.CheckoutStatus{commerce.ExecutionSucceeded: commerce.CheckoutPaid, commerce.ExecutionFailed: commerce.CheckoutPaymentFailed}
		if st, ok := settled[next.Status]; ok {
			if _, err := tx.Exec(ctx, `UPDATE commerce_checkouts SET status = $2 WHERE id = $1`, checkout, string(st)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return commerce.Execution{}, "", err
	}
	if refused != nil {
		return commerce.Execution{}, "", refused
	}
	return out, result, nil
}

// sweepBatchSize caps one sweep tick's work. Under steady state the
// overdue cohort per tick (the workflow runs this every minute against
// a 30-minute deadline) is small; this only matters after a backlog
// forms (a worker outage, say), bounding one tick's transaction instead
// of however large the backlog has grown. Uncaught rows wait for the
// next tick, which the workflow already schedules.
const sweepBatchSize = 500

// SweepExecutions moves overdue pending executions to unknown. It locks
// only the executions (no mandate); a report racing it holds the
// mandate, then waits for the execution's lock, and sees unknown.
func (s *CommerceCheckoutStore) SweepExecutions(ctx context.Context, at time.Time) ([]commerce.Execution, error) {
	var out []commerce.Execution
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH overdue AS (
				SELECT id FROM commerce_executions
				WHERE status = 'pending' AND report_deadline <= $1
				ORDER BY report_deadline
				LIMIT $2
			)
			UPDATE commerce_executions SET status = 'unknown', updated_at = $1
			WHERE id IN (SELECT id FROM overdue) RETURNING id`, at, sweepBatchSize)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			e, err := scanExecution(tx.QueryRow(ctx, executionSelect+` WHERE e.id = $1`, id))
			if err != nil {
				return err
			}
			if err := appendExecutionEvent(ctx, tx, e.ID, commerce.ExecutionEvent{Kind: commerce.EventSwept, Source: commerce.ReportSweep,
				Outcome: commerce.ExecutionUnknown, StatusAfter: commerce.ExecutionUnknown, AmountMinor: e.AmountMinor, Currency: e.Currency,
				Detail: "no outcome was reported before the deadline", At: at}); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func (s *CommerceCheckoutStore) ExecutionHistory(ctx context.Context, sc commerceapp.Scope, id commerce.ExecutionID) ([]commerce.ExecutionEvent, error) {
	if _, err := s.GetExecution(ctx, sc, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT sequence, kind, COALESCE(source, ''), COALESCE(outcome, ''), status_after, COALESCE(reference, ''),
		COALESCE(event_id, ''), COALESCE(amount_minor, 0), COALESCE(currency, ''), COALESCE(detail, ''), at
		FROM commerce_execution_events WHERE execution_id = $1 ORDER BY sequence`, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commerce.ExecutionEvent
	for rows.Next() {
		var ev commerce.ExecutionEvent
		var kind, source, outcome, after, currency string
		if err := rows.Scan(&ev.Sequence, &kind, &source, &outcome, &after, &ev.Reference, &ev.EventID, &ev.AmountMinor, &currency, &ev.Detail, &ev.At); err != nil {
			return nil, err
		}
		ev.Kind, ev.Source, ev.Outcome = commerce.ExecutionEventKind(kind), commerce.ReportSource(source), commerce.ExecutionStatus(outcome)
		ev.StatusAfter, ev.Currency = commerce.ExecutionStatus(after), currencyOf(currency)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *CommerceCheckoutStore) ListExecutions(ctx context.Context, sc commerceapp.Scope, f commerceapp.ExecutionFilter) ([]commerce.Execution, string, error) {
	w := &where{}
	w.add("e.org_id = ?", string(sc.OrgID))
	w.add("e.merchant_id = ?", string(sc.MerchantID))
	if f.Status != "" {
		w.add("e.status = ?", string(f.Status))
	}
	if f.MandateID != "" {
		w.add("e.mandate_id = ?", string(f.MandateID))
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(e.created_at, e.id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	rows, err := s.pool.Query(ctx, executionSelect+` WHERE `+w.sql()+` ORDER BY e.created_at DESC, e.id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing executions: %w", err)
	}
	defer rows.Close()
	var out []commerce.Execution
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		next = encodeCursor(out[f.Limit-1].CreatedAt, string(out[f.Limit-1].ID))
	}
	return out, next, nil
}

func (s *CommerceCheckoutStore) ListCheckouts(ctx context.Context, sc commerceapp.Scope, f commerceapp.CheckoutFilter, now time.Time) ([]commerce.Checkout, string, error) {
	w := &where{}
	w.add("c.org_id = ?", string(sc.OrgID))
	w.add("c.merchant_id = ?", string(sc.MerchantID))
	if f.MandateID != "" {
		w.add("c.mandate_id = ?", string(f.MandateID))
	}
	if f.CustomerID != "" {
		w.add("c.customer_id = ?", string(f.CustomerID))
	}
	if f.Status != "" {
		p := w.next(now)
		w.add(effectiveCheckoutStatus(p)+" = ?", string(f.Status))
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(c.created_at, c.id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	rows, err := s.pool.Query(ctx, checkoutSelect+` WHERE `+w.sql()+` ORDER BY c.created_at DESC, c.id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing checkouts: %w", err)
	}
	defer rows.Close()
	var out []commerce.Checkout
	for rows.Next() {
		c, err := scanCheckout(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		next = encodeCursor(out[f.Limit-1].CreatedAt, string(out[f.Limit-1].ID))
	}
	return out, next, nil
}

func (s *CommerceCheckoutStore) MandateUsage(ctx context.Context, sc commerceapp.Scope, ids []commerce.MandateID, now time.Time) (map[commerce.MandateID]commerce.MandateUsage, error) {
	out := map[commerce.MandateID]commerce.MandateUsage{}
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = string(id)
		out[id] = commerce.MandateUsage{}
	}
	rows, err := s.pool.Query(ctx, `SELECT c.mandate_id, count(*), COALESCE(sum(c.total_minor), 0),
			COALESCE(sum(c.total_minor) FILTER (WHERE c.status = 'paid'), 0)
		FROM commerce_checkouts c LEFT JOIN commerce_authorizations a ON a.checkout_id = c.id
		WHERE c.mandate_id = ANY($1) AND `+countsAgainstMandate+` AND c.org_id = $3 AND c.merchant_id = $4
		GROUP BY c.mandate_id`, list, now, string(sc.OrgID), string(sc.MerchantID))
	if err != nil {
		return nil, fmt.Errorf("reading mandate usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var u commerce.MandateUsage
		if err := rows.Scan(&id, &u.Purchases, &u.SpentMinor, &u.PaidMinor); err != nil {
			return nil, err
		}
		out[commerce.MandateID(id)] = u
	}
	return out, rows.Err()
}

func currencyOf(s string) domain.Currency { return domain.Currency(s) }
