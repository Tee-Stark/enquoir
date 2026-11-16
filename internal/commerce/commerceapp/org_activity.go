package commerceapp

import (
	"context"
	"fmt"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
)

// OrgCheckoutFilter narrows an organisation-wide list of checkouts,
// across every one of its merchant integrations. Zero fields don't
// filter; lists are newest first.
type OrgCheckoutFilter struct {
	MandateID  commerce.MandateID
	CustomerID commerce.CustomerID
	AgentID    commerce.AgentID
	Status     commerce.CheckoutStatus
	Currency   domain.Currency
	From, To   time.Time // CreatedAt in [From, To)
	Limit      int
	Cursor     string
}

// OrgExecutionFilter narrows an organisation-wide list of executions,
// across every one of its merchant integrations. Zero fields don't
// filter; lists are newest first.
type OrgExecutionFilter struct {
	Status    commerce.ExecutionStatus
	MandateID commerce.MandateID
	AgentID   commerce.AgentID
	Currency  domain.Currency
	From, To  time.Time // CreatedAt in [From, To)
	Limit     int
	Cursor    string
}

// CommerceOrgReader reads commerce records across an entire
// organisation, not scoped to one merchant integration - the dashboard's
// global view, parallel to treasury's PaymentReader/AttemptLog. This is
// deliberately a separate, read-only interface from CheckoutStore/
// ExecutionStore: those are keyed by Scope{OrgID, MerchantID}, and
// MerchantID is also used as a write-path lock key (lockMandateRow,
// lockCheckoutsMandate) - making it optional there would be a
// correctness hazard for mutating paths. This interface can never be
// wired into a write path by construction.
type CommerceOrgReader interface {
	ListCheckoutsForOrg(ctx context.Context, org domain.OrgID, f OrgCheckoutFilter, now time.Time) ([]commerce.Checkout, string, error)
	ListExecutionsForOrg(ctx context.Context, org domain.OrgID, f OrgExecutionFilter) ([]commerce.Execution, string, error)
	GetCheckoutForOrg(ctx context.Context, org domain.OrgID, id commerce.CheckoutID) (commerce.Checkout, error)
	GetExecutionForOrg(ctx context.Context, org domain.OrgID, id commerce.ExecutionID) (commerce.Execution, error)
	ExecutionHistoryForOrg(ctx context.Context, org domain.OrgID, id commerce.ExecutionID) ([]commerce.ExecutionEvent, error)
}

// period rejects a from/to range that is empty or backwards. Mirrors
// internal/app/history.go's period() - that one is unexported in
// package app, so it can't be reused directly from here.
func period(from, to time.Time) error {
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return &app.ValidationError{Field: "from", Message: "from must be before to"}
	}
	return nil
}

// OrgCheckoutPage is a page of an organisation's checkouts.
type OrgCheckoutPage struct {
	Checkouts  []CheckoutView
	NextCursor string
}

// OrgExecutionPage is a page of an organisation's executions.
type OrgExecutionPage struct {
	Executions []commerce.Execution
	NextCursor string
}

// ListOrgCheckouts lists every checkout across an organisation's
// merchant integrations, newest first - the dashboard's commerce
// activity view.
type ListOrgCheckouts struct {
	Reader CommerceOrgReader
	Clock  app.Clock
}

func (l *ListOrgCheckouts) Run(ctx context.Context, org domain.OrgID, f OrgCheckoutFilter) (OrgCheckoutPage, error) {
	lf, err := ListFilter{Limit: f.Limit}.normalised()
	if err != nil {
		return OrgCheckoutPage{}, err
	}
	f.Limit = lf.Limit
	if err := period(f.From, f.To); err != nil {
		return OrgCheckoutPage{}, err
	}
	if f.Status != "" && !checkoutStatuses[f.Status] {
		return OrgCheckoutPage{}, &app.ValidationError{Field: "status", Message: fmt.Sprintf("unknown checkout status %q", f.Status)}
	}
	if f.Currency != "" && !f.Currency.Supported() {
		return OrgCheckoutPage{}, &app.ValidationError{Field: "currency", Message: fmt.Sprintf("unsupported currency %q", f.Currency)}
	}
	now := l.Clock.Now()
	list, next, err := l.Reader.ListCheckoutsForOrg(ctx, org, f, now)
	if err != nil {
		return OrgCheckoutPage{}, err
	}
	page := OrgCheckoutPage{NextCursor: next, Checkouts: make([]CheckoutView, len(list))}
	for i, c := range list {
		page.Checkouts[i] = checkoutView(c, now)
	}
	return page, nil
}

// ListOrgExecutions lists every execution across an organisation's
// merchant integrations, newest first - the dashboard's commerce
// payments view.
type ListOrgExecutions struct{ Reader CommerceOrgReader }

func (l *ListOrgExecutions) Run(ctx context.Context, org domain.OrgID, f OrgExecutionFilter) (OrgExecutionPage, error) {
	lf, err := ListFilter{Limit: f.Limit}.normalised()
	if err != nil {
		return OrgExecutionPage{}, err
	}
	f.Limit = lf.Limit
	if err := period(f.From, f.To); err != nil {
		return OrgExecutionPage{}, err
	}
	if f.Status != "" {
		if f.Status, err = commerce.ParseExecutionStatus(string(f.Status)); err != nil {
			return OrgExecutionPage{}, invalid("status", err)
		}
	}
	if f.Currency != "" && !f.Currency.Supported() {
		return OrgExecutionPage{}, &app.ValidationError{Field: "currency", Message: fmt.Sprintf("unsupported currency %q", f.Currency)}
	}
	list, next, err := l.Reader.ListExecutionsForOrg(ctx, org, f)
	if err != nil {
		return OrgExecutionPage{}, err
	}
	return OrgExecutionPage{Executions: list, NextCursor: next}, nil
}

// GetOrgCheckout fetches one checkout belonging to the organisation,
// regardless of which merchant integration it's under.
type GetOrgCheckout struct {
	Reader CommerceOrgReader
	Clock  app.Clock
}

func (g *GetOrgCheckout) Run(ctx context.Context, org domain.OrgID, id commerce.CheckoutID) (CheckoutView, error) {
	c, err := g.Reader.GetCheckoutForOrg(ctx, org, id)
	if err != nil {
		return CheckoutView{}, err
	}
	return checkoutView(c, g.Clock.Now()), nil
}

// GetOrgExecution fetches one execution belonging to the organisation,
// with its history, regardless of which merchant integration it's under.
type GetOrgExecution struct{ Reader CommerceOrgReader }

func (g *GetOrgExecution) Run(ctx context.Context, org domain.OrgID, id commerce.ExecutionID) (ExecutionDetail, error) {
	e, err := g.Reader.GetExecutionForOrg(ctx, org, id)
	if err != nil {
		return ExecutionDetail{}, err
	}
	h, err := g.Reader.ExecutionHistoryForOrg(ctx, org, id)
	if err != nil {
		return ExecutionDetail{}, err
	}
	return ExecutionDetail{Execution: e, History: h}, nil
}
