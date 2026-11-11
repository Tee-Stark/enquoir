package commerceapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
)

var (
	// ErrCustomerUnknown means the customer named is not one of the
	// calling merchant's - it may exist under another merchant, which
	// this merchant cannot know.
	ErrCustomerUnknown = errors.New("the customer is not one this merchant registered")

	// ErrCustomerDisabled means the customer is the merchant's, but
	// disabled: nothing new can be bound to them until enabled.
	ErrCustomerDisabled = errors.New("the customer is disabled")
)

// FundingMethodPage is a page of a customer's bindings.
type FundingMethodPage struct {
	Methods    []commerce.FundingMethod
	NextCursor string
}

// FundingMethodStore persists funding-method bindings, scoped like the
// customers they belong to.
type FundingMethodStore interface {
	// RegisterFundingMethod stores f unless its merchant already has a
	// binding with f's reference; either way it returns the stored one.
	RegisterFundingMethod(ctx context.Context, f commerce.FundingMethod) (stored commerce.FundingMethod, created bool, err error)
	GetFundingMethod(ctx context.Context, s Scope, id commerce.FundingMethodID) (commerce.FundingMethod, error)
	// ListFundingMethods lists one customer's bindings, newest first.
	ListFundingMethods(ctx context.Context, s Scope, customer commerce.CustomerID, f ListFilter) (FundingMethodPage, error)
	SetFundingMethodStatus(ctx context.Context, s Scope, id commerce.FundingMethodID, status commerce.Status, at time.Time) (changed bool, err error)
}

// FundingMethodCommand registers a binding: the merchant's own
// reference for a customer's saved payment method, its PSP, and what to
// show the customer. Never a PSP token or any card data.
type FundingMethodCommand struct {
	CustomerID commerce.CustomerID
	Reference  string
	Provider   string
	Brand      string
	LastFour   string
}

const auditTargetFundingMethod = "commerce_funding_method"

// fundingInvalid names the request field at fault without repeating the
// value: it may be card data, and errors end up in logs and responses.
func fundingInvalid(field string, err error) error {
	return &app.ValidationError{Field: field, Message: err.Error()}
}

// RegisterFundingMethod registers a binding for one of the merchant's
// active customers, or resolves the one already registered under that
// reference. It grants no spending authority: only the customer's
// mandate does.
type RegisterFundingMethod struct {
	Methods   FundingMethodStore
	Customers CustomerStore
	IDs       app.IDGenerator
	Clock     app.Clock
	Audit     *app.Audit
}

func (r *RegisterFundingMethod) Run(ctx context.Context, p MerchantPrincipal, cmd FundingMethodCommand) (commerce.FundingMethod, bool, error) {
	customer := commerce.CustomerID(strings.TrimSpace(string(cmd.CustomerID)))
	if customer == "" {
		return commerce.FundingMethod{}, false, &app.ValidationError{Field: "customer_id", Message: "customer_id is required"}
	}
	ref, err := commerce.NewFundingReference(cmd.Reference)
	if err != nil {
		return commerce.FundingMethod{}, false, fundingInvalid("merchant_funding_method_id", err)
	}
	provider, err := commerce.NewProvider(cmd.Provider)
	if err != nil {
		return commerce.FundingMethod{}, false, fundingInvalid("provider", err)
	}
	display, err := commerce.NewFundingDisplay(cmd.Brand, cmd.LastFour)
	if err != nil {
		var ferr *commerce.FieldError
		if errors.As(err, &ferr) {
			return commerce.FundingMethod{}, false, fundingInvalid("display."+ferr.Field, ferr.Err)
		}
		return commerce.FundingMethod{}, false, err
	}

	c, err := r.Customers.GetCustomer(ctx, p.Scope(), customer)
	if errors.Is(err, app.ErrNotFound) {
		return commerce.FundingMethod{}, false, ErrCustomerUnknown
	}
	if err != nil {
		return commerce.FundingMethod{}, false, err
	}
	if !c.Status.IsActive() {
		return commerce.FundingMethod{}, false, ErrCustomerDisabled
	}

	f := commerce.FundingMethod{
		ID: commerce.FundingMethodID(r.IDs.NewID("fmb")), MerchantID: p.MerchantID, OrgID: p.OrgID, CustomerID: c.ID,
		Reference: ref, Provider: provider, Display: display, Status: commerce.Active, CreatedAt: r.Clock.Now(),
	}
	stored, created, err := r.Methods.RegisterFundingMethod(ctx, f)
	if err != nil {
		return commerce.FundingMethod{}, false, fmt.Errorf("registering funding method: %w", err)
	}
	if !created {
		if !stored.SameBinding(f) {
			return commerce.FundingMethod{}, false, ErrRegistrationConflict
		}
		return stored, false, nil
	}
	r.Audit.Record(ctx, p.OrgID, app.AuditFundingMethodRegistered, auditTargetFundingMethod, string(stored.ID), map[string]string{
		"merchant_id": string(p.MerchantID), "customer_id": string(c.ID),
		"merchant_funding_method_id": string(ref), "provider": string(provider),
	})
	return stored, true, nil
}

// GetFundingMethod fetches one of the merchant's bindings.
type GetFundingMethod struct{ Methods FundingMethodStore }

func (g *GetFundingMethod) Run(ctx context.Context, p MerchantPrincipal, id commerce.FundingMethodID) (commerce.FundingMethod, error) {
	return g.Methods.GetFundingMethod(ctx, p.Scope(), id)
}

// ListFundingMethods lists one of the merchant's customers' bindings. A
// customer that isn't the merchant's is app.ErrNotFound.
type ListFundingMethods struct {
	Methods   FundingMethodStore
	Customers CustomerStore
}

func (l *ListFundingMethods) Run(ctx context.Context, p MerchantPrincipal, customer commerce.CustomerID, f ListFilter) (FundingMethodPage, error) {
	f, err := f.normalised()
	if err != nil {
		return FundingMethodPage{}, err
	}
	if _, err := l.Customers.GetCustomer(ctx, p.Scope(), customer); err != nil {
		return FundingMethodPage{}, err
	}
	return l.Methods.ListFundingMethods(ctx, p.Scope(), customer, f)
}

// SetFundingMethodStatus disables or enables one of the merchant's
// bindings - when the customer removes the card from the merchant, say.
type SetFundingMethodStatus struct {
	Methods FundingMethodStore
	Clock   app.Clock
	Audit   *app.Audit
}

func (s *SetFundingMethodStatus) Run(ctx context.Context, p MerchantPrincipal, id commerce.FundingMethodID, status commerce.Status) (commerce.FundingMethod, error) {
	status, err := commerce.ParseStatus(string(status))
	if err != nil {
		return commerce.FundingMethod{}, invalid("status", err)
	}
	changed, err := s.Methods.SetFundingMethodStatus(ctx, p.Scope(), id, status, s.Clock.Now())
	if err != nil {
		return commerce.FundingMethod{}, err
	}
	if changed {
		action := statusAction(status, app.AuditFundingMethodEnabled, app.AuditFundingMethodDisabled)
		s.Audit.Record(ctx, p.OrgID, action, auditTargetFundingMethod, string(id), map[string]string{"merchant_id": string(p.MerchantID)})
	}
	return s.Methods.GetFundingMethod(ctx, p.Scope(), id)
}
