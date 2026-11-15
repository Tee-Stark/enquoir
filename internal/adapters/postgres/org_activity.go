package postgres

import (
	"context"
	"fmt"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

var _ commerceapp.CommerceOrgReader = (*CommerceCheckoutStore)(nil)

// ListCheckoutsForOrg lists an organisation's checkouts across every
// one of its merchant integrations - the dashboard's commerce activity
// view, parallel to ListPayments' org-wide scope. Unlike ListCheckouts,
// there is no merchant_id predicate at all.
func (s *CommerceCheckoutStore) ListCheckoutsForOrg(ctx context.Context, org domain.OrgID, f commerceapp.OrgCheckoutFilter, now time.Time) ([]commerce.Checkout, string, error) {
	w := &where{}
	w.add("c.org_id = ?", string(org))
	if f.MandateID != "" {
		w.add("c.mandate_id = ?", string(f.MandateID))
	}
	if f.CustomerID != "" {
		w.add("c.customer_id = ?", string(f.CustomerID))
	}
	if f.AgentID != "" {
		w.add("c.agent_id = ?", string(f.AgentID))
	}
	if f.Status != "" {
		p := w.next(now)
		w.add(effectiveCheckoutStatus(p)+" = ?", string(f.Status))
	}
	if f.Currency != "" {
		w.add("c.currency = ?", string(f.Currency))
	}
	if !f.From.IsZero() {
		w.add("c.created_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		w.add("c.created_at < ?", f.To)
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
		return nil, "", fmt.Errorf("listing checkouts for org: %w", err)
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

// ListExecutionsForOrg lists an organisation's executions across every
// one of its merchant integrations. commerce_executions has no agent_id
// column (unlike commerce_checkouts), so an agent filter joins back to
// the owning checkout.
func (s *CommerceCheckoutStore) ListExecutionsForOrg(ctx context.Context, org domain.OrgID, f commerceapp.OrgExecutionFilter) ([]commerce.Execution, string, error) {
	w := &where{}
	w.add("e.org_id = ?", string(org))
	if f.Status != "" {
		w.add("e.status = ?", string(f.Status))
	}
	if f.MandateID != "" {
		w.add("e.mandate_id = ?", string(f.MandateID))
	}
	if f.Currency != "" {
		w.add("e.currency = ?", string(f.Currency))
	}
	if !f.From.IsZero() {
		w.add("e.created_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		w.add("e.created_at < ?", f.To)
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(e.created_at, e.id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	q := executionSelect
	if f.AgentID != "" {
		// Reuse executionSelect verbatim rather than hand-duplicating its
		// column list, so this doesn't drift if that query ever changes.
		// commerce_checkouts.id is the execution's own FK target and both
		// rows are written under the same Scope (checkouts.go), so a
		// mismatched join isn't reachable - the extra c.org_id = e.org_id
		// condition is defense-in-depth, not a fix for a real gap.
		q = executionSelect + ` JOIN commerce_checkouts c ON c.id = e.checkout_id AND c.org_id = e.org_id`
		w.add("c.agent_id = ?", string(f.AgentID))
	}
	rows, err := s.pool.Query(ctx, q+` WHERE `+w.sql()+` ORDER BY e.created_at DESC, e.id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing executions for org: %w", err)
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

// GetCheckoutForOrg fetches one of an organisation's checkouts,
// regardless of which merchant integration it belongs to.
func (s *CommerceCheckoutStore) GetCheckoutForOrg(ctx context.Context, org domain.OrgID, id commerce.CheckoutID) (commerce.Checkout, error) {
	return scanCheckout(s.pool.QueryRow(ctx, checkoutSelect+`
		WHERE c.org_id = $1 AND c.id = $2`, string(org), string(id)))
}

// GetExecutionForOrg fetches one of an organisation's executions,
// regardless of which merchant integration it belongs to.
func (s *CommerceCheckoutStore) GetExecutionForOrg(ctx context.Context, org domain.OrgID, id commerce.ExecutionID) (commerce.Execution, error) {
	return scanExecution(s.pool.QueryRow(ctx, executionSelect+` WHERE e.org_id = $1 AND e.id = $2`,
		string(org), string(id)))
}

// ExecutionHistoryForOrg reads an execution's append-only report
// history, checking it belongs to org without requiring a merchant
// scope.
func (s *CommerceCheckoutStore) ExecutionHistoryForOrg(ctx context.Context, org domain.OrgID, id commerce.ExecutionID) ([]commerce.ExecutionEvent, error) {
	if _, err := s.GetExecutionForOrg(ctx, org, id); err != nil {
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
