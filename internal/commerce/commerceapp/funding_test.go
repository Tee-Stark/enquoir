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

type fundingFixture struct {
	*directoryFixture
	register *commerceapp.RegisterFundingMethod
	get      *commerceapp.GetFundingMethod
	list     *commerceapp.ListFundingMethods
	status   *commerceapp.SetFundingMethodStatus
}

func newFundingFixture() *fundingFixture {
	d := newDirectoryFixture()
	ids := &apptest.IDs{}
	audit := &app.Audit{Log: d.audit, IDs: ids, Clock: d.clock}
	methods := commercetest.NewFundingMethods()
	customers := d.getCustomer.Customers
	return &fundingFixture{
		directoryFixture: d,
		register:         &commerceapp.RegisterFundingMethod{Methods: methods, Customers: customers, IDs: ids, Clock: d.clock, Audit: audit},
		get:              &commerceapp.GetFundingMethod{Methods: methods},
		list:             &commerceapp.ListFundingMethods{Methods: methods, Customers: customers},
		status:           &commerceapp.SetFundingMethodStatus{Methods: methods, Clock: d.clock, Audit: audit},
	}
}

func visa(customer commerce.CustomerID, ref string) commerceapp.FundingMethodCommand {
	return commerceapp.FundingMethodCommand{CustomerID: customer, Reference: ref, Provider: "stripe", Brand: "visa", LastFour: "4242"}
}

func (f *fundingFixture) bind(t *testing.T, m commerceapp.MerchantPrincipal, cmd commerceapp.FundingMethodCommand) commerce.FundingMethod {
	t.Helper()
	fm, _, err := f.register.Run(context.Background(), m, cmd)
	if err != nil {
		t.Fatalf("binding %q: %v", cmd.Reference, err)
	}
	return fm
}

func TestRegisterFundingMethod_CreatesThenResolvesTheSameBinding(t *testing.T) {
	f := newFundingFixture()
	ctx := context.Background()
	c := f.customer(t, shopA1, "cust_42", "ada@example.com")

	first, created, err := f.register.Run(ctx, shopA1, commerceapp.FundingMethodCommand{
		CustomerID: c.ID, Reference: " mfm_123 ", Provider: " Stripe ", Brand: "Visa", LastFour: "4242",
	})
	if err != nil || !created {
		t.Fatalf("first: created %v, %v", created, err)
	}
	if first.ID == "" || first.CustomerID != c.ID || first.Reference != "mfm_123" || first.Provider != "stripe" ||
		first.Display != (commerce.FundingDisplay{Brand: "visa", LastFour: "4242"}) || first.Status != commerce.Active ||
		first.MerchantID != shopA1.MerchantID || first.OrgID != orgA {
		t.Errorf("binding = %+v", first)
	}
	again, created, err := f.register.Run(ctx, shopA1, visa(c.ID, "mfm_123"))
	if err != nil || created || again.ID != first.ID {
		t.Errorf("repeat: %+v created %v, %v; want the same binding", again, created, err)
	}

	events := f.audit.All()
	var registered []app.AuditEvent
	for _, e := range events {
		if e.Action == app.AuditFundingMethodRegistered {
			registered = append(registered, e)
		}
	}
	if len(registered) != 1 || registered[0].TargetID != string(first.ID) || registered[0].Details["customer_id"] != string(c.ID) ||
		registered[0].Details["provider"] != "stripe" || registered[0].Details["merchant_funding_method_id"] != "mfm_123" {
		t.Errorf("audit = %+v", registered)
	}
}

// One reference is one payment method. Repeating it with a different
// customer, provider or display details is a conflict.
func TestRegisterFundingMethod_RefusesDifferentDetailsForAKnownReference(t *testing.T) {
	f := newFundingFixture()
	ada := f.customer(t, shopA1, "ada", "ada@example.com")
	bob := f.customer(t, shopA1, "bob", "bob@example.com")
	f.bind(t, shopA1, visa(ada.ID, "mfm_1"))

	for name, cmd := range map[string]commerceapp.FundingMethodCommand{
		"another customer":  visa(bob.ID, "mfm_1"),
		"another provider":  {CustomerID: ada.ID, Reference: "mfm_1", Provider: "adyen", Brand: "visa", LastFour: "4242"},
		"another brand":     {CustomerID: ada.ID, Reference: "mfm_1", Provider: "stripe", Brand: "mastercard", LastFour: "4242"},
		"another last four": {CustomerID: ada.ID, Reference: "mfm_1", Provider: "stripe", Brand: "visa", LastFour: "0005"},
	} {
		if _, _, err := f.register.Run(context.Background(), shopA1, cmd); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
			t.Errorf("%s: %v, want a registration conflict", name, err)
		}
	}
}

// Only an active customer of the calling merchant can be bound. Another
// merchant's customer is unknown, not forbidden.
func TestRegisterFundingMethod_NeedsAnActiveCustomerOfThisMerchant(t *testing.T) {
	f := newFundingFixture()
	ctx := context.Background()
	mine := f.customer(t, shopA1, "cust_42", "ada@example.com")
	sibling := f.customer(t, shopA2, "cust_42", "ada@example.com")
	stranger := f.customer(t, shopB, "cust_42", "ada@example.com")

	for _, other := range []commerce.CustomerID{sibling.ID, stranger.ID, "cus_made_up"} {
		if _, _, err := f.register.Run(ctx, shopA1, visa(other, "mfm_x")); !errors.Is(err, commerceapp.ErrCustomerUnknown) {
			t.Errorf("binding customer %s: %v, want customer unknown", other, err)
		}
	}
	f.customerStatus.Run(ctx, shopA1, mine.ID, commerce.Disabled)
	if _, _, err := f.register.Run(ctx, shopA1, visa(mine.ID, "mfm_x")); !errors.Is(err, commerceapp.ErrCustomerDisabled) {
		t.Errorf("binding a disabled customer: %v, want customer disabled", err)
	}
}

func TestRegisterFundingMethod_ValidatesAndRefusesCardData(t *testing.T) {
	f := newFundingFixture()
	c := f.customer(t, shopA1, "cust_42", "ada@example.com")
	cases := []struct {
		name  string
		cmd   commerceapp.FundingMethodCommand
		field string
	}{
		{"no customer", visa("", "mfm_1"), "customer_id"},
		{"no reference", visa(c.ID, ""), "merchant_funding_method_id"},
		{"a card number as the reference", visa(c.ID, "4242 4242 4242 4242"), "merchant_funding_method_id"},
		{"no provider", commerceapp.FundingMethodCommand{CustomerID: c.ID, Reference: "mfm_1", Brand: "visa"}, "provider"},
		{"a card number as the provider", commerceapp.FundingMethodCommand{CustomerID: c.ID, Reference: "mfm_1", Provider: "4242424242424242", Brand: "visa"}, "provider"},
		{"no brand", commerceapp.FundingMethodCommand{CustomerID: c.ID, Reference: "mfm_1", Provider: "stripe"}, "display.brand"},
		{"a whole card as last four", commerceapp.FundingMethodCommand{CustomerID: c.ID, Reference: "mfm_1", Provider: "stripe", Brand: "visa", LastFour: "4242424242424242"}, "display.last_four"},
	}
	for _, tc := range cases {
		_, _, err := f.register.Run(context.Background(), shopA1, tc.cmd)
		var verr *app.ValidationError
		if !errors.As(err, &verr) || verr.Field != tc.field {
			t.Errorf("%s: %v, want a validation error on %s", tc.name, err, tc.field)
		}
		if strings.Contains(err.Error(), "4242424242424242") || strings.Contains(err.Error(), "4242 4242") {
			t.Errorf("%s: the error repeats the card number: %v", tc.name, err)
		}
	}
	if page, _ := f.list.Run(context.Background(), shopA1, c.ID, commerceapp.ListFilter{}); len(page.Methods) != 0 {
		t.Errorf("refused registrations stored %d bindings", len(page.Methods))
	}
}

func TestFundingMethods_AreScopedToTheirMerchant(t *testing.T) {
	f := newFundingFixture()
	ctx := context.Background()
	c := f.customer(t, shopA1, "cust_42", "ada@example.com")
	fm := f.bind(t, shopA1, visa(c.ID, "mfm_1"))

	// The same reference under another merchant is its own binding.
	theirs := f.customer(t, shopA2, "cust_42", "ada@example.com")
	if other := f.bind(t, shopA2, visa(theirs.ID, "mfm_1")); other.ID == fm.ID {
		t.Error("one reference under two merchants gave one binding")
	}
	for _, other := range []commerceapp.MerchantPrincipal{shopA2, shopB} {
		if _, err := f.get.Run(ctx, other, fm.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s reading A1's binding: %v, want not found", other.MerchantID, err)
		}
		if _, err := f.status.Run(ctx, other, fm.ID, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s disabling A1's binding: %v, want not found", other.MerchantID, err)
		}
		if _, err := f.list.Run(ctx, other, c.ID, commerceapp.ListFilter{}); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s listing A1's customer's bindings: %v, want not found", other.MerchantID, err)
		}
	}
	if got, _ := f.get.Run(ctx, shopA1, fm.ID); got.Status != commerce.Active {
		t.Errorf("after the others' attempts: %+v", got)
	}
}

func TestListFundingMethods_ListsOneCustomersBindings(t *testing.T) {
	f := newFundingFixture()
	ctx := context.Background()
	ada := f.customer(t, shopA1, "ada", "ada@example.com")
	bob := f.customer(t, shopA1, "bob", "bob@example.com")
	a1 := f.bind(t, shopA1, visa(ada.ID, "mfm_a1"))
	f.clock.Advance(1)
	a2 := f.bind(t, shopA1, commerceapp.FundingMethodCommand{CustomerID: ada.ID, Reference: "mfm_a2", Provider: "stripe", Brand: "paypal"})
	f.bind(t, shopA1, visa(bob.ID, "mfm_b1"))
	f.status.Run(ctx, shopA1, a1.ID, commerce.Disabled)

	page, err := f.list.Run(ctx, shopA1, ada.ID, commerceapp.ListFilter{})
	if err != nil || len(page.Methods) != 2 || page.Methods[0].ID != a2.ID || page.Methods[1].ID != a1.ID {
		t.Fatalf("ada's bindings = %+v, %v; want both, newest first", page.Methods, err)
	}
	active, _ := f.list.Run(ctx, shopA1, ada.ID, commerceapp.ListFilter{Status: commerce.Active})
	if len(active.Methods) != 1 || active.Methods[0].ID != a2.ID {
		t.Errorf("ada's active bindings = %+v", active.Methods)
	}
	first, _ := f.list.Run(ctx, shopA1, ada.ID, commerceapp.ListFilter{Limit: 1})
	rest, _ := f.list.Run(ctx, shopA1, ada.ID, commerceapp.ListFilter{Limit: 1, Cursor: first.NextCursor})
	if len(first.Methods) != 1 || first.NextCursor == "" || len(rest.Methods) != 1 || rest.Methods[0].ID != a1.ID {
		t.Errorf("paged: %+v then %+v", first, rest)
	}
	var verr *app.ValidationError
	if _, err := f.list.Run(ctx, shopA1, ada.ID, commerceapp.ListFilter{Status: "gone"}); !errors.As(err, &verr) {
		t.Errorf("unknown status: %v", err)
	}
}

func TestSetFundingMethodStatus_DisablesAndEnables(t *testing.T) {
	f := newFundingFixture()
	ctx := context.Background()
	c := f.customer(t, shopA1, "cust_42", "ada@example.com")
	fm := f.bind(t, shopA1, visa(c.ID, "mfm_1"))

	got, err := f.status.Run(ctx, shopA1, fm.ID, commerce.Disabled)
	if err != nil || got.Status != commerce.Disabled || got.DisabledAt == nil {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	f.status.Run(ctx, shopA1, fm.ID, commerce.Disabled)
	// Re-registering a disabled binding resolves it, still disabled.
	if again, created, err := f.register.Run(ctx, shopA1, visa(c.ID, "mfm_1")); err != nil || created || again.Status != commerce.Disabled {
		t.Errorf("re-registering: %+v %v %v", again, created, err)
	}
	if got, err := f.status.Run(ctx, shopA1, fm.ID, commerce.Active); err != nil || got.Status != commerce.Active || got.DisabledAt != nil {
		t.Fatalf("enable: %+v, %v", got, err)
	}
	var actions []string
	for _, e := range f.audit.All() {
		if strings.HasPrefix(e.Action, "commerce.funding_method.") {
			actions = append(actions, e.Action)
		}
	}
	want := []string{app.AuditFundingMethodRegistered, app.AuditFundingMethodDisabled, app.AuditFundingMethodEnabled}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", actions, want)
	}
}
