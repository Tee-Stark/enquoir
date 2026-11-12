package commerce

import (
	"errors"
	"strings"
	"testing"
	"time"

	"enquoir/internal/domain"
)

func laptop() CheckoutSnapshot {
	return CheckoutSnapshot{
		OrgID: "org_1", MerchantID: "mi_1", MandateID: "mdt_1", ExternalID: "checkout_987",
		CustomerID: "cus_1", AgentID: "cagt_1", FundingMethodID: "fmb_1", Currency: domain.USD,
		Items: []CheckoutItem{
			{ProductID: "laptop_001", Name: "Lenovo ThinkPad", Category: "electronics", Quantity: 1, UnitPriceMinor: 130000},
			{ProductID: "sleeve_002", Name: "Sleeve", Category: "electronics", Quantity: 2, UnitPriceMinor: 2000},
		},
		ShippingMinor: 500, TaxMinor: 0, FeesMinor: 100, TotalMinor: 134600,
	}
}

func TestNewCheckoutSnapshot_NormalisesAndChecksTheTotal(t *testing.T) {
	in := laptop()
	in.Currency = "usd"
	in.Items[0].Category = " Electronics "
	in.Items[0].Name = "  Lenovo ThinkPad "
	got, err := NewCheckoutSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency != domain.USD || got.Items[0].Category != "electronics" || got.Items[0].Name != "Lenovo ThinkPad" {
		t.Errorf("snapshot = %+v", got)
	}

	bad := map[string]struct {
		change func(*CheckoutSnapshot)
		field  string
	}{
		"total does not add up": {func(c *CheckoutSnapshot) { c.TotalMinor++ }, "total_minor"},
		"no items":              {func(c *CheckoutSnapshot) { c.Items = nil }, "items"},
		"zero quantity":         {func(c *CheckoutSnapshot) { c.Items[0].Quantity = 0 }, "items[0].quantity"},
		"negative price":        {func(c *CheckoutSnapshot) { c.Items[1].UnitPriceMinor = -1 }, "items[1].unit_price_minor"},
		"no category":           {func(c *CheckoutSnapshot) { c.Items[0].Category = "" }, "items[0].category"},
		"no name":               {func(c *CheckoutSnapshot) { c.Items[0].Name = " " }, "items[0].name"},
		"bad product":           {func(c *CheckoutSnapshot) { c.Items[0].ProductID = "a b" }, "items[0].product_id"},
		"negative shipping":     {func(c *CheckoutSnapshot) { c.ShippingMinor = -5 }, "shipping_minor"},
		"NGN":                   {func(c *CheckoutSnapshot) { c.Currency = "NGN" }, "currency"},
		"free": {func(c *CheckoutSnapshot) {
			c.Items = []CheckoutItem{{ProductID: "x", Name: "x", Category: "x", Quantity: 1}}
			c.ShippingMinor, c.FeesMinor, c.TotalMinor = 0, 0, 0
		}, "total_minor"},
		"absurd price":   {func(c *CheckoutSnapshot) { c.Items[0].UnitPriceMinor = maxCheckoutAmount + 1 }, "items[0].unit_price_minor"},
		"too many items": {func(c *CheckoutSnapshot) { c.Items = make([]CheckoutItem, maxCheckoutItems+1) }, "items"},
		"no external ID": {func(c *CheckoutSnapshot) { c.ExternalID = "" }, "external_id"},
	}
	for name, tc := range bad {
		c := laptop()
		tc.change(&c)
		_, err := NewCheckoutSnapshot(c)
		var ferr *FieldError
		if !errors.As(err, &ferr) || ferr.Field != tc.field {
			t.Errorf("%s: %v, want an error on %s", name, err, tc.field)
		}
	}
}

// The hash binds every financially relevant field.
func TestCheckoutSnapshot_HashBindsEveryField(t *testing.T) {
	base, _ := NewCheckoutSnapshot(laptop())
	if again, _ := NewCheckoutSnapshot(laptop()); again.Hash() != base.Hash() || !strings.HasPrefix(base.Hash(), "sha256:") {
		t.Fatal("the hash is not stable")
	}
	changes := map[string]func(*CheckoutSnapshot){
		"mandate":    func(c *CheckoutSnapshot) { c.MandateID = "mdt_2" },
		"customer":   func(c *CheckoutSnapshot) { c.CustomerID = "cus_2" },
		"agent":      func(c *CheckoutSnapshot) { c.AgentID = "cagt_2" },
		"card":       func(c *CheckoutSnapshot) { c.FundingMethodID = "fmb_2" },
		"external":   func(c *CheckoutSnapshot) { c.ExternalID = "checkout_988" },
		"product":    func(c *CheckoutSnapshot) { c.Items[0].ProductID = "laptop_002" },
		"name":       func(c *CheckoutSnapshot) { c.Items[0].Name = "Other" },
		"category":   func(c *CheckoutSnapshot) { c.Items[0].Category = "books" },
		"item order": func(c *CheckoutSnapshot) { c.Items[0], c.Items[1] = c.Items[1], c.Items[0] },
		"quantity and price": func(c *CheckoutSnapshot) {
			c.Items[1].Quantity, c.Items[1].UnitPriceMinor = 4, 1000
		},
		"shipping into tax": func(c *CheckoutSnapshot) { c.ShippingMinor, c.TaxMinor = 0, 500 },
		"prices alone":      func(c *CheckoutSnapshot) { c.Items[0].UnitPriceMinor -= 1000; c.Items[1].UnitPriceMinor += 500 },
		"merchant":          func(c *CheckoutSnapshot) { c.MerchantID = "mi_2" },
	}
	for name, change := range changes {
		c := laptop()
		change(&c)
		snap, err := NewCheckoutSnapshot(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if snap.Hash() == base.Hash() {
			t.Errorf("changing the %s left the hash unchanged", name)
		}
	}
	back, err := ParseCheckoutSnapshot(base.Canonical())
	if err != nil || back.Hash() != base.Hash() {
		t.Errorf("round trip: %v", err)
	}
}

func mandateFor(t *testing.T, mode ApprovalMode, count int, categories ...string) Mandate {
	t.Helper()
	scope, err := NewMandateScope(domain.USD, 150000, 300000, count, categories)
	if err != nil {
		t.Fatal(err)
	}
	terms, err := NewMandateTerms(MandateParties{OrgID: "org_1", MerchantID: "mi_1", CustomerID: "cus_1", AgentID: "cagt_1", FundingMethodID: "fmb_1"},
		mode, scope, now.Add(30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	return Mandate{ID: "mdt_1", Terms: terms}
}

func codes(ds []Denial) string {
	var out []string
	for _, d := range ds {
		out = append(out, string(d.Code))
	}
	return strings.Join(out, ",")
}

func TestEvaluateCheckout(t *testing.T) {
	m := mandateFor(t, Autonomous, 3, "electronics")
	snap, _ := NewCheckoutSnapshot(laptop())
	if got := EvaluateCheckout(m, MandateActive, snap, MandateUsage{}); len(got) != 0 {
		t.Fatalf("within scope: %v", got)
	}

	cases := []struct {
		name   string
		status MandateStatus
		change func(*CheckoutSnapshot)
		usage  MandateUsage
		want   string
	}{
		{"revoked", MandateRevoked, nil, MandateUsage{}, "mandate_not_active"},
		{"revocation pending", MandateRevocationPending, nil, MandateUsage{}, "mandate_not_active"},
		{"suspended", MandateSuspended, nil, MandateUsage{}, "mandate_not_active"},
		{"another customer", MandateActive, func(c *CheckoutSnapshot) { c.CustomerID = "cus_2" }, MandateUsage{}, "customer_mismatch"},
		{"another agent", MandateActive, func(c *CheckoutSnapshot) { c.AgentID = "cagt_2" }, MandateUsage{}, "agent_mismatch"},
		{"another card", MandateActive, func(c *CheckoutSnapshot) { c.FundingMethodID = "fmb_2" }, MandateUsage{}, "funding_method_mismatch"},
		{"another currency", MandateActive, func(c *CheckoutSnapshot) { c.Currency = domain.EUR }, MandateUsage{}, "currency_mismatch"},
		{"over the per-purchase maximum", MandateActive, func(c *CheckoutSnapshot) {
			c.Items[0].UnitPriceMinor, c.TotalMinor = 150000, 154600
		}, MandateUsage{}, "over_transaction_limit"},
		{"a category not allowed", MandateActive, func(c *CheckoutSnapshot) { c.Items[1].Category = "books" }, MandateUsage{}, "category_not_allowed"},
		{"over the count", MandateActive, nil, MandateUsage{Purchases: 3, SpentMinor: 1}, "over_transaction_count"},
		{"over the budget", MandateActive, nil, MandateUsage{Purchases: 1, SpentMinor: 200000}, "over_budget"},
		{"exactly the budget", MandateActive, nil, MandateUsage{Purchases: 2, SpentMinor: 300000 - 134600}, ""},
	}
	for _, tc := range cases {
		c := laptop()
		if tc.change != nil {
			tc.change(&c)
		}
		s, err := NewCheckoutSnapshot(c)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := codes(EvaluateCheckout(m, tc.status, s, tc.usage)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	// Every failing reason is reported, not just the first.
	c := laptop()
	c.AgentID, c.Currency, c.Items[1].Category = "cagt_2", domain.GBP, "books"
	s, _ := NewCheckoutSnapshot(c)
	if got := codes(EvaluateCheckout(m, MandateExpired, s, MandateUsage{Purchases: 3, SpentMinor: 299999})); got !=
		"mandate_not_active,agent_mismatch,currency_mismatch,category_not_allowed,over_transaction_count,over_budget" {
		t.Errorf("all reasons: %s", got)
	}
	// No categories in the mandate: any category; no count: any number.
	open := mandateFor(t, Autonomous, 0)
	c = laptop()
	c.Items[1].Category = "books"
	s, _ = NewCheckoutSnapshot(c)
	if got := EvaluateCheckout(open, MandateActive, s, MandateUsage{Purchases: 1000}); len(got) != 0 {
		t.Errorf("an open mandate: %v", got)
	}
}

func TestCheckout_StatusAt(t *testing.T) {
	exp := now.Add(time.Hour)
	c := Checkout{Status: CheckoutAwaitingApproval, ApprovalExpiresAt: &exp}
	if c.StatusAt(now) != CheckoutAwaitingApproval || c.StatusAt(exp) != CheckoutExpired {
		t.Error("an awaiting checkout expires at its approval expiry")
	}
	c.Status = CheckoutAuthorized
	if c.StatusAt(exp.Add(time.Hour)) != CheckoutAuthorized {
		t.Error("an authorized checkout keeps its status")
	}
}
