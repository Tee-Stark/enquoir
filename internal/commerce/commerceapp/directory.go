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

// ErrRegistrationConflict means the merchant already registered that
// external ID with different details. Repeating a registration is safe;
// changing one is not done by repeating it.
var ErrRegistrationConflict = errors.New("this external ID is already registered with different details")

// Scope is the one merchant integration a commerce request acts for,
// and its organisation. Every customer and agent read and write is
// scoped by both: another merchant's record is app.ErrNotFound, even
// within one organisation.
type Scope struct {
	OrgID      domain.OrgID
	MerchantID commerce.MerchantID
}

// Scope is the namespace this merchant may read and write.
func (p MerchantPrincipal) Scope() Scope { return Scope{OrgID: p.OrgID, MerchantID: p.MerchantID} }

// ListFilter narrows a list of customers or agents. Zero fields don't
// filter; lists are newest first.
type ListFilter struct {
	ExternalID commerce.ExternalID
	Status     commerce.Status
	Limit      int
	Cursor     string
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

func (f ListFilter) normalised() (ListFilter, error) {
	switch {
	case f.Limit == 0:
		f.Limit = defaultPageSize
	case f.Limit < 1 || f.Limit > maxPageSize:
		return f, &app.ValidationError{Field: "limit", Message: fmt.Sprintf("limit must be 1 to %d", maxPageSize)}
	}
	if f.ExternalID != "" {
		ext, err := commerce.NewExternalID(string(f.ExternalID))
		if err != nil {
			return f, invalid("external_id", err)
		}
		f.ExternalID = ext
	}
	if f.Status != "" {
		st, err := commerce.ParseStatus(string(f.Status))
		if err != nil {
			return f, invalid("status", err)
		}
		f.Status = st
	}
	return f, nil
}

// CustomerPage and AgentPage are pages of a list; NextCursor is empty
// on the last.
type CustomerPage struct {
	Customers  []commerce.Customer
	NextCursor string
}

type AgentPage struct {
	Agents     []commerce.Agent
	NextCursor string
}

// CustomerStore persists a merchant's customers.
type CustomerStore interface {
	// RegisterCustomer stores c unless its merchant already has a
	// customer with c's external ID. Either way it returns the stored
	// customer; created reports which.
	RegisterCustomer(ctx context.Context, c commerce.Customer) (stored commerce.Customer, created bool, err error)
	ListCustomers(ctx context.Context, s Scope, f ListFilter) (CustomerPage, error)
	GetCustomer(ctx context.Context, s Scope, id commerce.CustomerID) (commerce.Customer, error)
	// SetCustomerStatus is SetIntegrationStatus's counterpart.
	SetCustomerStatus(ctx context.Context, s Scope, id commerce.CustomerID, status commerce.Status, at time.Time) (changed bool, err error)
}

// AgentStore persists a merchant's agents, as CustomerStore does its
// customers.
type AgentStore interface {
	RegisterAgent(ctx context.Context, a commerce.Agent) (stored commerce.Agent, created bool, err error)
	ListAgents(ctx context.Context, s Scope, f ListFilter) (AgentPage, error)
	GetAgent(ctx context.Context, s Scope, id commerce.AgentID) (commerce.Agent, error)
	SetAgentStatus(ctx context.Context, s Scope, id commerce.AgentID, status commerce.Status, at time.Time) (changed bool, err error)
}

const (
	auditTargetCustomer = "commerce_customer"
	auditTargetAgent    = "commerce_agent"
)

func statusAction(status commerce.Status, enabled, disabled string) string {
	if status == commerce.Disabled {
		return disabled
	}
	return enabled
}

// RegisterCustomer registers a merchant's customer, or resolves the one
// it already registered under that external ID. Email is where consent
// links will go; it is normalised, and kept out of the audit log.
type RegisterCustomer struct {
	Customers CustomerStore
	IDs       app.IDGenerator
	Clock     app.Clock
	Audit     *app.Audit
}

func (r *RegisterCustomer) Run(ctx context.Context, p MerchantPrincipal, externalID, email string) (commerce.Customer, bool, error) {
	ext, err := commerce.NewExternalID(externalID)
	if err != nil {
		return commerce.Customer{}, false, invalid("external_id", err)
	}
	addr, err := domain.NewEmail(email)
	if err != nil {
		return commerce.Customer{}, false, invalid("email", err)
	}
	c := commerce.Customer{
		ID: commerce.CustomerID(r.IDs.NewID("cus")), MerchantID: p.MerchantID, OrgID: p.OrgID,
		ExternalID: ext, Email: addr, Status: commerce.Active, CreatedAt: r.Clock.Now(),
	}
	stored, created, err := r.Customers.RegisterCustomer(ctx, c)
	if err != nil {
		return commerce.Customer{}, false, fmt.Errorf("registering customer: %w", err)
	}
	if !created {
		if stored.Email != addr {
			return commerce.Customer{}, false, ErrRegistrationConflict
		}
		return stored, false, nil
	}
	r.Audit.Record(ctx, p.OrgID, app.AuditCommerceCustomerRegistered, auditTargetCustomer, string(stored.ID),
		map[string]string{"merchant_id": string(p.MerchantID), "external_id": string(ext)})
	return stored, true, nil
}

// ListCustomers lists a merchant's customers, newest first.
type ListCustomers struct{ Customers CustomerStore }

func (l *ListCustomers) Run(ctx context.Context, p MerchantPrincipal, f ListFilter) (CustomerPage, error) {
	f, err := f.normalised()
	if err != nil {
		return CustomerPage{}, err
	}
	return l.Customers.ListCustomers(ctx, p.Scope(), f)
}

// GetCustomer fetches one of a merchant's customers.
type GetCustomer struct{ Customers CustomerStore }

func (g *GetCustomer) Run(ctx context.Context, p MerchantPrincipal, id commerce.CustomerID) (commerce.Customer, error) {
	return g.Customers.GetCustomer(ctx, p.Scope(), id)
}

// SetCustomerStatus disables or enables one of a merchant's customers.
type SetCustomerStatus struct {
	Customers CustomerStore
	Clock     app.Clock
	Audit     *app.Audit
}

func (s *SetCustomerStatus) Run(ctx context.Context, p MerchantPrincipal, id commerce.CustomerID, status commerce.Status) (commerce.Customer, error) {
	status, err := commerce.ParseStatus(string(status))
	if err != nil {
		return commerce.Customer{}, invalid("status", err)
	}
	changed, err := s.Customers.SetCustomerStatus(ctx, p.Scope(), id, status, s.Clock.Now())
	if err != nil {
		return commerce.Customer{}, err
	}
	if changed {
		action := statusAction(status, app.AuditCommerceCustomerEnabled, app.AuditCommerceCustomerDisabled)
		s.Audit.Record(ctx, p.OrgID, action, auditTargetCustomer, string(id), map[string]string{"merchant_id": string(p.MerchantID)})
	}
	return s.Customers.GetCustomer(ctx, p.Scope(), id)
}

// RegisterAgent registers an AI agent the merchant runs, or resolves the
// one it already registered under that external ID.
type RegisterAgent struct {
	Agents AgentStore
	IDs    app.IDGenerator
	Clock  app.Clock
	Audit  *app.Audit
}

func (r *RegisterAgent) Run(ctx context.Context, p MerchantPrincipal, externalID, name string) (commerce.Agent, bool, error) {
	ext, err := commerce.NewExternalID(externalID)
	if err != nil {
		return commerce.Agent{}, false, invalid("external_id", err)
	}
	name, err = domain.NewName(name)
	if err != nil {
		return commerce.Agent{}, false, invalid("name", err)
	}
	a := commerce.Agent{
		ID: commerce.AgentID(r.IDs.NewID("cagt")), MerchantID: p.MerchantID, OrgID: p.OrgID,
		ExternalID: ext, Name: name, Status: commerce.Active, CreatedAt: r.Clock.Now(),
	}
	stored, created, err := r.Agents.RegisterAgent(ctx, a)
	if err != nil {
		return commerce.Agent{}, false, fmt.Errorf("registering agent: %w", err)
	}
	if !created {
		if stored.Name != name {
			return commerce.Agent{}, false, ErrRegistrationConflict
		}
		return stored, false, nil
	}
	r.Audit.Record(ctx, p.OrgID, app.AuditCommerceAgentRegistered, auditTargetAgent, string(stored.ID),
		map[string]string{"merchant_id": string(p.MerchantID), "external_id": string(ext), "name": name})
	return stored, true, nil
}

// ListAgents lists a merchant's agents, newest first.
type ListAgents struct{ Agents AgentStore }

func (l *ListAgents) Run(ctx context.Context, p MerchantPrincipal, f ListFilter) (AgentPage, error) {
	f, err := f.normalised()
	if err != nil {
		return AgentPage{}, err
	}
	return l.Agents.ListAgents(ctx, p.Scope(), f)
}

// GetAgent fetches one of a merchant's agents.
type GetAgent struct{ Agents AgentStore }

func (g *GetAgent) Run(ctx context.Context, p MerchantPrincipal, id commerce.AgentID) (commerce.Agent, error) {
	return g.Agents.GetAgent(ctx, p.Scope(), id)
}

// SetAgentStatus disables or enables one of a merchant's agents.
type SetAgentStatus struct {
	Agents AgentStore
	Clock  app.Clock
	Audit  *app.Audit
}

func (s *SetAgentStatus) Run(ctx context.Context, p MerchantPrincipal, id commerce.AgentID, status commerce.Status) (commerce.Agent, error) {
	status, err := commerce.ParseStatus(string(status))
	if err != nil {
		return commerce.Agent{}, invalid("status", err)
	}
	changed, err := s.Agents.SetAgentStatus(ctx, p.Scope(), id, status, s.Clock.Now())
	if err != nil {
		return commerce.Agent{}, err
	}
	if changed {
		action := statusAction(status, app.AuditCommerceAgentEnabled, app.AuditCommerceAgentDisabled)
		s.Audit.Record(ctx, p.OrgID, action, auditTargetAgent, string(id), map[string]string{"merchant_id": string(p.MerchantID)})
	}
	return s.Agents.GetAgent(ctx, p.Scope(), id)
}
