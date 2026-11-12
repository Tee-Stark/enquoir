package commerceapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
)

// ExecutionApplier applies a report to an execution, under its
// mandate's lock: commerce.Execution.Apply.
type ExecutionApplier func(e commerce.Execution) (commerce.Execution, commerce.ReportResult, error)

// ExecutionFilter narrows a list of executions. Zero fields don't
// filter; lists are newest first.
type ExecutionFilter struct {
	Status    commerce.ExecutionStatus
	MandateID commerce.MandateID
	Limit     int
	Cursor    string
}

// CheckoutFilter narrows a list of checkouts. Status is the effective
// status (an unredeemed grant past its expiry is expired).
type CheckoutFilter struct {
	MandateID  commerce.MandateID
	CustomerID commerce.CustomerID
	Status     commerce.CheckoutStatus
	Limit      int
	Cursor     string
}

// ExecutionStore is the part of the checkout store that tracks
// executions' outcomes, and the lists the dashboard reads.
type ExecutionStore interface {
	// ReportExecution locks the execution's mandate and the execution,
	// then: for a webhook report whose event ID it has seen, returns the
	// execution unchanged (ReportDuplicate) if it was the same report for
	// the same execution, else commerce.ErrEventConflict; otherwise calls
	// apply. It records every report in the execution's history - a
	// refused one too, before returning apply's error - and on an applied
	// definitive outcome settles the checkout (paid or payment_failed).
	ReportExecution(ctx context.Context, s Scope, id commerce.ExecutionID, r commerce.ExecutionReport, at time.Time, apply ExecutionApplier) (commerce.Execution, commerce.ReportResult, error)
	// SweepExecutions moves every execution pending past its report
	// deadline at at to unknown, recording it, and returns them.
	SweepExecutions(ctx context.Context, at time.Time) ([]commerce.Execution, error)
	ExecutionHistory(ctx context.Context, s Scope, id commerce.ExecutionID) ([]commerce.ExecutionEvent, error)
	ListExecutions(ctx context.Context, s Scope, f ExecutionFilter) ([]commerce.Execution, string, error)
	ListCheckouts(ctx context.Context, s Scope, f CheckoutFilter, now time.Time) ([]commerce.Checkout, string, error)
}

// MandateUsageReader reports what counts against mandates at now.
type MandateUsageReader interface {
	MandateUsage(ctx context.Context, s Scope, ids []commerce.MandateID, now time.Time) (map[commerce.MandateID]commerce.MandateUsage, error)
}

const auditTargetExecution = "commerce_execution"

// ExecutionView is an execution and what the last report did.
type ExecutionView struct {
	commerce.Execution
	Result commerce.ReportResult
}

// ReportExecutionOutcome records the merchant's report of a payment's
// outcome - its own (direct), a PSP webhook it verified, or a
// reconciliation after looking the payment up. The outcome is the
// merchant's word; Enquoir checks it against the execution.
type ReportExecutionOutcome struct {
	Checkouts CheckoutStore
	Clock     app.Clock
	Audit     *app.Audit
}

func (u *ReportExecutionOutcome) Run(ctx context.Context, p MerchantPrincipal, id commerce.ExecutionID, report commerce.ExecutionReport) (ExecutionView, error) {
	r, err := commerce.NewExecutionReport(report)
	if err != nil {
		return ExecutionView{}, fieldErr(err)
	}
	now := u.Clock.Now()
	e, result, err := u.Checkouts.ReportExecution(ctx, p.Scope(), id, r, now, func(e commerce.Execution) (commerce.Execution, commerce.ReportResult, error) {
		return e.Apply(r, now)
	})
	details := map[string]string{
		"merchant_id": string(p.MerchantID), "source": string(r.Source), "outcome": string(r.Outcome),
		"provider_payment_reference": r.ProviderPaymentReference, "provider_event_id": r.EventID,
	}
	switch {
	case errors.Is(err, commerce.ErrReportMismatch), errors.Is(err, commerce.ErrOutcomeConflict),
		errors.Is(err, commerce.ErrReconciliationRequired), errors.Is(err, commerce.ErrEventConflict):
		details["reason"] = err.Error()
		u.Audit.Record(ctx, p.OrgID, app.AuditExecutionReportRefused, auditTargetExecution, string(id), details)
		return ExecutionView{}, err
	case err != nil:
		return ExecutionView{}, err
	}
	if result == commerce.ReportApplied {
		action := map[commerce.ExecutionStatus]string{
			commerce.ExecutionSucceeded: app.AuditExecutionSucceeded, commerce.ExecutionFailed: app.AuditExecutionFailed,
			commerce.ExecutionUnknown: app.AuditExecutionUnknown,
		}[e.Status]
		if r.Source == commerce.ReportReconciliation {
			action = app.AuditExecutionReconciled
		}
		u.Audit.Record(ctx, p.OrgID, action, auditTargetExecution, string(id), details)
	}
	return ExecutionView{Execution: e, Result: result}, nil
}

// SweepExecutions presumes unknown every execution still pending past
// its report deadline: its merchant may have crashed mid-charge. Unknown
// is not failed - the amount keeps counting until it is reconciled.
type SweepExecutions struct {
	Checkouts CheckoutStore
	Clock     app.Clock
	Audit     *app.Audit
}

func (s *SweepExecutions) Run(ctx context.Context) (int, error) {
	swept, err := s.Checkouts.SweepExecutions(ctx, s.Clock.Now())
	if err != nil {
		return 0, fmt.Errorf("sweeping executions: %w", err)
	}
	for _, e := range swept {
		s.Audit.Record(ctx, e.OrgID, app.AuditExecutionUnknown, auditTargetExecution, string(e.ID), map[string]string{
			"merchant_id": string(e.MerchantID), "source": string(commerce.ReportSweep),
			"reason": "no outcome was reported before the deadline",
		})
	}
	return len(swept), nil
}

// ExecutionDetail is an execution with its history.
type ExecutionDetail struct {
	commerce.Execution
	History []commerce.ExecutionEvent
}

// ExecutionPage is a page of executions.
type ExecutionPage struct {
	Executions []commerce.Execution
	NextCursor string
}

// ListExecutions lists the merchant's executions: status=unknown is the
// reconciliation queue.
type ListExecutions struct{ Checkouts CheckoutStore }

func (l *ListExecutions) Run(ctx context.Context, p MerchantPrincipal, f ExecutionFilter) (ExecutionPage, error) {
	lf, err := ListFilter{Limit: f.Limit}.normalised()
	if err != nil {
		return ExecutionPage{}, err
	}
	f.Limit = lf.Limit
	if f.Status != "" {
		if f.Status, err = commerce.ParseExecutionStatus(string(f.Status)); err != nil {
			return ExecutionPage{}, invalid("status", err)
		}
	}
	list, next, err := l.Checkouts.ListExecutions(ctx, p.Scope(), f)
	if err != nil {
		return ExecutionPage{}, err
	}
	return ExecutionPage{Executions: list, NextCursor: next}, nil
}

// CheckoutPage is a page of checkouts.
type CheckoutPage struct {
	Checkouts  []CheckoutView
	NextCursor string
}

var checkoutStatuses = map[commerce.CheckoutStatus]bool{
	commerce.CheckoutAuthorized: true, commerce.CheckoutAwaitingApproval: true, commerce.CheckoutDenied: true,
	commerce.CheckoutRejected: true, commerce.CheckoutCanceled: true, commerce.CheckoutExpired: true,
	commerce.CheckoutRedeemed: true, commerce.CheckoutRevoked: true, commerce.CheckoutPaid: true, commerce.CheckoutPaymentFailed: true,
}

// ListCheckouts lists the merchant's checkouts, newest first.
type ListCheckouts struct {
	Checkouts CheckoutStore
	Clock     app.Clock
}

func (l *ListCheckouts) Run(ctx context.Context, p MerchantPrincipal, f CheckoutFilter) (CheckoutPage, error) {
	lf, err := ListFilter{Limit: f.Limit}.normalised()
	if err != nil {
		return CheckoutPage{}, err
	}
	f.Limit = lf.Limit
	if f.Status != "" && !checkoutStatuses[f.Status] {
		return CheckoutPage{}, &app.ValidationError{Field: "status", Message: fmt.Sprintf("unknown checkout status %q", f.Status)}
	}
	now := l.Clock.Now()
	list, next, err := l.Checkouts.ListCheckouts(ctx, p.Scope(), f, now)
	if err != nil {
		return CheckoutPage{}, err
	}
	page := CheckoutPage{NextCursor: next, Checkouts: make([]CheckoutView, len(list))}
	for i, c := range list {
		page.Checkouts[i] = checkoutView(c, now)
	}
	return page, nil
}

// MerchantForDashboard is a member's read-only view of one of their
// organisation's merchant integrations: the principal to list its
// checkouts, mandates and executions with. Another organisation's
// integration is app.ErrNotFound.
type MerchantForDashboard struct{ Integrations IntegrationStore }

func (m *MerchantForDashboard) Run(ctx context.Context, org domain.OrgID, id commerce.MerchantID) (MerchantPrincipal, error) {
	i, err := m.Integrations.GetIntegration(ctx, org, id)
	if err != nil {
		return MerchantPrincipal{}, err
	}
	return MerchantPrincipal{MerchantID: i.ID, MerchantName: i.Name, OrgID: i.OrgID}, nil
}
