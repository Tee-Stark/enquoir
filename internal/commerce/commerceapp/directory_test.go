package commerceapp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/commerce/commerceapp/commercetest"
)

// Two merchants of one organisation (two storefronts), and a merchant
// of another: each is its own namespace.
var (
	shopA1 = commerceapp.MerchantPrincipal{MerchantID: "mi_a1", MerchantName: "A1", OrgID: orgA, KeyID: "mkey_a1"}
	shopA2 = commerceapp.MerchantPrincipal{MerchantID: "mi_a2", MerchantName: "A2", OrgID: orgA, KeyID: "mkey_a2"}
	shopB  = commerceapp.MerchantPrincipal{MerchantID: "mi_b", MerchantName: "B", OrgID: orgB, KeyID: "mkey_b"}
)

type directoryFixture struct {
	clock *apptest.Clock
	audit *apptest.AuditLog

	registerCustomer *commerceapp.RegisterCustomer
	listCustomers    *commerceapp.ListCustomers
	getCustomer      *commerceapp.GetCustomer
	customerStatus   *commerceapp.SetCustomerStatus

	registerAgent *commerceapp.RegisterAgent
	listAgents    *commerceapp.ListAgents
	getAgent      *commerceapp.GetAgent
	agentStatus   *commerceapp.SetAgentStatus
}

func newDirectoryFixture() *directoryFixture {
	clock, log, ids := apptest.NewClock(), apptest.NewAuditLog(), &apptest.IDs{}
	audit := &app.Audit{Log: log, IDs: ids, Clock: clock}
	customers, agents := commercetest.NewCustomers(), commercetest.NewAgents()
	return &directoryFixture{
		clock: clock, audit: log,
		registerCustomer: &commerceapp.RegisterCustomer{Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		listCustomers:    &commerceapp.ListCustomers{Customers: customers},
		getCustomer:      &commerceapp.GetCustomer{Customers: customers},
		customerStatus:   &commerceapp.SetCustomerStatus{Customers: customers, Clock: clock, Audit: audit},
		registerAgent:    &commerceapp.RegisterAgent{Agents: agents, IDs: ids, Clock: clock, Audit: audit},
		listAgents:       &commerceapp.ListAgents{Agents: agents},
		getAgent:         &commerceapp.GetAgent{Agents: agents},
		agentStatus:      &commerceapp.SetAgentStatus{Agents: agents, Clock: clock, Audit: audit},
	}
}

func (f *directoryFixture) customer(t *testing.T, m commerceapp.MerchantPrincipal, ext, email string) commerce.Customer {
	t.Helper()
	c, _, err := f.registerCustomer.Run(context.Background(), m, ext, email)
	if err != nil {
		t.Fatalf("registering customer %q: %v", ext, err)
	}
	return c
}

func (f *directoryFixture) agent(t *testing.T, m commerceapp.MerchantPrincipal, ext, name string) commerce.Agent {
	t.Helper()
	a, _, err := f.registerAgent.Run(context.Background(), m, ext, name)
	if err != nil {
		t.Fatalf("registering agent %q: %v", ext, err)
	}
	return a
}

func TestRegisterCustomer_CreatesThenResolvesTheSameRecord(t *testing.T) {
	f := newDirectoryFixture()
	ctx := app.WithActor(context.Background(), app.Actor{MerchantID: string(shopA1.MerchantID), MerchantName: shopA1.MerchantName})

	first, created, err := f.registerCustomer.Run(ctx, shopA1, " cust_42 ", " Ada@Example.com ")
	if err != nil || !created {
		t.Fatalf("first registration: created %v, %v", created, err)
	}
	if first.ExternalID != "cust_42" || first.Email != "ada@example.com" || first.Status != commerce.Active ||
		first.MerchantID != shopA1.MerchantID || first.OrgID != orgA || first.ID == "" {
		t.Errorf("customer = %+v", first)
	}

	again, created, err := f.registerCustomer.Run(ctx, shopA1, "cust_42", "ada@example.com")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeated registration: %+v created %v, %v; want the same record", again, created, err)
	}

	events := f.audit.All()
	if len(events) != 1 {
		t.Fatalf("audit = %+v, want one registration event", events)
	}
	e := events[0]
	if e.Action != app.AuditCommerceCustomerRegistered || e.OrgID != orgA || e.TargetID != string(first.ID) ||
		e.ActorMerchant != string(shopA1.MerchantID) || e.MerchantName != "A1" || e.Details["external_id"] != "cust_42" {
		t.Errorf("event = %+v", e)
	}
	// The email is personal data; the audit log does not need it.
	if strings.Contains(strings.Join([]string{e.Details["email"], e.Details["external_id"]}, ""), "ada@") {
		t.Errorf("the audit event carries the customer's email: %+v", e.Details)
	}
}

// Repeating a registration is safe; changing it is not done by
// repeating it. A different email for a known external ID is refused,
// not silently ignored or applied.
func TestRegisterCustomer_RefusesDifferentDetailsForAKnownExternalID(t *testing.T) {
	f := newDirectoryFixture()
	f.customer(t, shopA1, "cust_42", "ada@example.com")
	_, _, err := f.registerCustomer.Run(context.Background(), shopA1, "cust_42", "eve@example.com")
	if !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Fatalf("different email: %v, want a registration conflict", err)
	}
	got, _ := f.listCustomers.Run(context.Background(), shopA1, commerceapp.ListFilter{})
	if len(got.Customers) != 1 || got.Customers[0].Email != "ada@example.com" {
		t.Errorf("customers after the conflict = %+v", got.Customers)
	}
}

func TestRegisterCustomer_ValidatesItsFields(t *testing.T) {
	f := newDirectoryFixture()
	for _, tc := range []struct{ ext, email, field string }{
		{"", "ada@example.com", "external_id"},
		{"cust 42", "ada@example.com", "external_id"},
		{"cust_42", "", "email"},
		{"cust_42", "not-an-email", "email"},
	} {
		_, _, err := f.registerCustomer.Run(context.Background(), shopA1, tc.ext, tc.email)
		var verr *app.ValidationError
		if !errors.As(err, &verr) || verr.Field != tc.field {
			t.Errorf("(%q, %q): %v, want a validation error on %s", tc.ext, tc.email, err, tc.field)
		}
	}
}

// External IDs are the merchant's own namespace: the same one under
// two merchants - even two storefronts of one organisation - is two
// customers, and neither merchant can see the other's.
func TestCustomers_AreScopedToTheirMerchant(t *testing.T) {
	f := newDirectoryFixture()
	ctx := context.Background()
	a1 := f.customer(t, shopA1, "cust_42", "ada@example.com")
	a2 := f.customer(t, shopA2, "cust_42", "ada@example.com")
	b := f.customer(t, shopB, "cust_42", "ada@example.com")
	if a1.ID == a2.ID || a1.ID == b.ID || a2.ID == b.ID {
		t.Fatalf("one external ID under three merchants gave IDs %s, %s, %s; want three records", a1.ID, a2.ID, b.ID)
	}

	for _, other := range []commerceapp.MerchantPrincipal{shopA2, shopB} {
		if _, err := f.getCustomer.Run(ctx, other, a1.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s reading A1's customer: %v, want not found", other.MerchantID, err)
		}
		if _, err := f.customerStatus.Run(ctx, other, a1.ID, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s disabling A1's customer: %v, want not found", other.MerchantID, err)
		}
	}
	page, err := f.listCustomers.Run(ctx, shopA1, commerceapp.ListFilter{})
	if err != nil || len(page.Customers) != 1 || page.Customers[0].ID != a1.ID {
		t.Errorf("A1's list = %+v, %v; want only its own", page.Customers, err)
	}
	if got, _ := f.getCustomer.Run(ctx, shopA1, a1.ID); got.Status != commerce.Active {
		t.Errorf("A1's customer after the others' attempts: %+v", got)
	}
}

func TestSetCustomerStatus_DisablesAndEnables(t *testing.T) {
	f := newDirectoryFixture()
	ctx := context.Background()
	c := f.customer(t, shopA1, "cust_42", "ada@example.com")

	got, err := f.customerStatus.Run(ctx, shopA1, c.ID, commerce.Disabled)
	if err != nil || got.Status != commerce.Disabled || got.DisabledAt == nil {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	// Registering a disabled customer again resolves it, still disabled.
	again, created, err := f.registerCustomer.Run(ctx, shopA1, "cust_42", "ada@example.com")
	if err != nil || created || again.Status != commerce.Disabled {
		t.Errorf("re-registering a disabled customer: %+v created %v, %v", again, created, err)
	}
	f.customerStatus.Run(ctx, shopA1, c.ID, commerce.Disabled) // no change, no event
	got, err = f.customerStatus.Run(ctx, shopA1, c.ID, commerce.Active)
	if err != nil || got.Status != commerce.Active || got.DisabledAt != nil {
		t.Fatalf("enable: %+v, %v", got, err)
	}
	var actions []string
	for _, e := range f.audit.All() {
		actions = append(actions, e.Action)
	}
	want := []string{app.AuditCommerceCustomerRegistered, app.AuditCommerceCustomerDisabled, app.AuditCommerceCustomerEnabled}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", actions, want)
	}
	var verr *app.ValidationError
	if _, err := f.customerStatus.Run(ctx, shopA1, c.ID, "deleted"); !errors.As(err, &verr) {
		t.Errorf("unknown status: %v, want a validation error", err)
	}
}

func TestListCustomers_FiltersAndPages(t *testing.T) {
	f := newDirectoryFixture()
	ctx := context.Background()
	for _, ext := range []string{"c1", "c2", "c3", "c4", "c5"} {
		f.customer(t, shopA1, ext, ext+"@example.com")
		f.clock.Advance(1)
	}
	c3, _, _ := f.registerCustomer.Run(ctx, shopA1, "c3", "c3@example.com")
	f.customerStatus.Run(ctx, shopA1, c3.ID, commerce.Disabled)

	byExt, err := f.listCustomers.Run(ctx, shopA1, commerceapp.ListFilter{ExternalID: " c3 "})
	if err != nil || len(byExt.Customers) != 1 || byExt.Customers[0].ID != c3.ID {
		t.Errorf("by external ID: %+v, %v", byExt.Customers, err)
	}
	disabled, _ := f.listCustomers.Run(ctx, shopA1, commerceapp.ListFilter{Status: commerce.Disabled})
	if len(disabled.Customers) != 1 || disabled.Customers[0].ID != c3.ID {
		t.Errorf("disabled: %+v", disabled.Customers)
	}

	var seen []string
	cursor := ""
	for range 4 {
		page, err := f.listCustomers.Run(ctx, shopA1, commerceapp.ListFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Customers {
			seen = append(seen, string(c.ExternalID))
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(seen, ",") != "c5,c4,c3,c2,c1" {
		t.Errorf("paged newest first: %v", seen)
	}

	for _, bad := range []commerceapp.ListFilter{{Limit: -1}, {Limit: 201}, {Status: "gone"}, {ExternalID: "a b"}} {
		var verr *app.ValidationError
		if _, err := f.listCustomers.Run(ctx, shopA1, bad); !errors.As(err, &verr) {
			t.Errorf("filter %+v: %v, want a validation error", bad, err)
		}
	}
}

func TestRegisterAgent_CreatesThenResolvesAndRefusesARename(t *testing.T) {
	f := newDirectoryFixture()
	ctx := context.Background()
	first, created, err := f.registerAgent.Run(ctx, shopA1, "shopping-assistant", "  Acme Shopping Assistant ")
	if err != nil || !created || first.Name != "Acme Shopping Assistant" || first.ExternalID != "shopping-assistant" ||
		first.MerchantID != shopA1.MerchantID || first.OrgID != orgA || first.Status != commerce.Active {
		t.Fatalf("first registration: %+v created %v, %v", first, created, err)
	}
	again, created, err := f.registerAgent.Run(ctx, shopA1, "shopping-assistant", "Acme Shopping Assistant")
	if err != nil || created || again.ID != first.ID {
		t.Errorf("repeated: %+v created %v, %v; want the same record", again, created, err)
	}
	if _, _, err := f.registerAgent.Run(ctx, shopA1, "shopping-assistant", "Another name"); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("a different name: %v, want a registration conflict", err)
	}
	var verr *app.ValidationError
	if _, _, err := f.registerAgent.Run(ctx, shopA1, "shopping-assistant", " "); !errors.As(err, &verr) || verr.Field != "name" {
		t.Errorf("blank name: %v, want a validation error on name", err)
	}
	if _, _, err := f.registerAgent.Run(ctx, shopA1, "another-agent", "4242.4242.4242.4242"); !errors.As(err, &verr) || verr.Field != "name" {
		t.Errorf("name looks like a card number: %v, want a validation error on name", err)
	}
	if e := f.audit.All(); len(e) != 1 || e[0].Action != app.AuditCommerceAgentRegistered || e[0].Details["name"] != "Acme Shopping Assistant" {
		t.Errorf("audit = %+v", e)
	}
}

func TestAgents_AreScopedToTheirMerchantAndCanBeDisabled(t *testing.T) {
	f := newDirectoryFixture()
	ctx := context.Background()
	a1 := f.agent(t, shopA1, "bot", "Bot")
	a2 := f.agent(t, shopA2, "bot", "Bot")
	if a1.ID == a2.ID {
		t.Fatal("one external ID under two merchants gave one agent")
	}
	if _, err := f.getAgent.Run(ctx, shopB, a1.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another organisation's merchant reading the agent: %v, want not found", err)
	}
	if _, err := f.agentStatus.Run(ctx, shopA2, a1.ID, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a sibling storefront disabling the agent: %v, want not found", err)
	}
	got, err := f.agentStatus.Run(ctx, shopA1, a1.ID, commerce.Disabled)
	if err != nil || got.Status != commerce.Disabled {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	page, err := f.listAgents.Run(ctx, shopA1, commerceapp.ListFilter{Status: commerce.Disabled})
	if err != nil || len(page.Agents) != 1 || page.Agents[0].ID != a1.ID {
		t.Errorf("disabled agents: %+v, %v", page.Agents, err)
	}
	if got, err := f.agentStatus.Run(ctx, shopA1, a1.ID, commerce.Active); err != nil || got.Status != commerce.Active {
		t.Errorf("enable: %+v, %v", got, err)
	}
}
