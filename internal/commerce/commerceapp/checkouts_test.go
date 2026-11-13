package commerceapp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/commerce/commerceapp/commercetest"
	"enquoir/internal/policy"
)

type checkoutsFixture struct {
	*mandatesFixture
	submit   *commerceapp.SubmitCheckout
	getCk    *commerceapp.GetCheckout
	cancel   *commerceapp.CancelCheckout
	viewCk   *commerceapp.ViewCheckoutApproval
	decideCk *commerceapp.DecideCheckoutApproval
}

func newCheckoutsFixture(t *testing.T) *checkoutsFixture {
	t.Helper()
	m := newMandatesFixture(t)
	store := commercetest.NewCheckouts(m.store, m.tokens)
	customers := m.getCustomer.Customers
	return &checkoutsFixture{
		mandatesFixture: m,
		submit:          &commerceapp.SubmitCheckout{Checkouts: store, Customers: customers, Emails: m.emails, IDs: m.ids, Secrets: m.secrets, Clock: m.clock, Audit: m.auditor},
		getCk:           &commerceapp.GetCheckout{Checkouts: store, Clock: m.clock},
		cancel:          &commerceapp.CancelCheckout{Checkouts: store, Clock: m.clock, Audit: m.auditor},
		viewCk:          &commerceapp.ViewCheckoutApproval{Tokens: m.tokens, Checkouts: store, Clock: m.clock},
		decideCk:        &commerceapp.DecideCheckoutApproval{Tokens: m.tokens, Checkouts: store, IDs: m.ids, Clock: m.clock, Audit: m.auditor},
	}
}

// mandate approves a mandate of the given mode for the fixture's
// customer, agent and card: USD 1,500 per purchase, 3,000 in total, 3
// purchases, electronics only.
func (f *checkoutsFixture) mandate(t *testing.T, ext string, mode commerce.ApprovalMode) commerce.Mandate {
	t.Helper()
	c := f.cmd(ext)
	c.ApprovalMode = string(mode)
	r, _, err := f.request.Run(context.Background(), shopA1, c)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.decide.Run(context.Background(), f.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	return *d.Mandate
}

// laptop is a USD 1,346 electronics checkout under m.
func (f *checkoutsFixture) laptop(m commerce.Mandate, ext string) commerceapp.CheckoutCommand {
	return commerceapp.CheckoutCommand{
		ExternalID: ext, MandateID: m.ID, CustomerID: f.ada.ID, AgentID: f.bot.ID, FundingMethodID: f.card.ID, Currency: "USD",
		Items: []commerce.CheckoutItem{
			{ProductID: "laptop_001", Name: "Lenovo ThinkPad", Category: "electronics", Quantity: 1, UnitPriceMinor: 130000},
			{ProductID: "sleeve_002", Name: "Sleeve", Category: "electronics", Quantity: 2, UnitPriceMinor: 2000},
		},
		ShippingMinor: 500, FeesMinor: 100, TotalMinor: 134600,
	}
}

func priced(c commerceapp.CheckoutCommand, total int64) commerceapp.CheckoutCommand {
	c.Items = []commerce.CheckoutItem{{ProductID: "p", Name: "P", Category: "electronics", Quantity: 1, UnitPriceMinor: total}}
	c.ShippingMinor, c.TaxMinor, c.FeesMinor, c.TotalMinor = 0, 0, 0, total
	return c
}

func (f *checkoutsFixture) mustSubmit(t *testing.T, cmd commerceapp.CheckoutCommand) commerceapp.CheckoutView {
	t.Helper()
	c, _, err := f.submit.Run(context.Background(), shopA1, cmd)
	if err != nil {
		t.Fatalf("submitting %s: %v", cmd.ExternalID, err)
	}
	return c
}

func codesOf(c commerceapp.CheckoutView) string {
	var out []string
	for _, d := range c.Denials {
		out = append(out, string(d.Code))
	}
	return strings.Join(out, ",")
}

func TestSubmitCheckout_AutonomousWithinScopeIsAuthorized(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	c, created, err := f.submit.Run(context.Background(), shopA1, f.laptop(m, "checkout_1"))
	if err != nil || !created {
		t.Fatalf("submit: %v %v", created, err)
	}
	if c.Status != commerce.CheckoutAuthorized || len(c.Denials) != 0 || c.Snapshot.MerchantID != shopA1.MerchantID || !strings.HasPrefix(c.Snapshot.Hash(), "sha256:") {
		t.Errorf("checkout = %+v", c)
	}
	again, created, err := f.submit.Run(context.Background(), shopA1, f.laptop(m, "checkout_1"))
	if err != nil || created || again.ID != c.ID {
		t.Errorf("repeat: %+v %v %v", again, created, err)
	}
	changed := f.laptop(m, "checkout_1")
	changed.Items[1].Quantity, changed.TotalMinor = 3, 136600
	if _, _, err := f.submit.Run(context.Background(), shopA1, changed); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("a changed checkout under the same ID: %v, want a conflict", err)
	}
	if a := f.actions("commerce.checkout."); strings.Join(a, ",") != app.AuditCheckoutAuthorized {
		t.Errorf("audit = %v", a)
	}
}

func TestSubmitCheckout_DeniedCheckoutsAreRecordedWithEveryReason(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	cmd := f.laptop(m, "checkout_1")
	cmd.Currency = "EUR"
	cmd.Items[1].Category = "books"
	c := f.mustSubmit(t, cmd)
	if c.Status != commerce.CheckoutDenied || codesOf(c) != "currency_mismatch,category_not_allowed" {
		t.Errorf("checkout = %s %s", c.Status, codesOf(c))
	}
	if got, err := f.getCk.Run(context.Background(), shopA1, c.ID); err != nil || got.Status != commerce.CheckoutDenied || len(got.Denials) != 2 {
		t.Errorf("stored: %+v %v", got, err)
	}
	if a := f.actions("commerce.checkout."); strings.Join(a, ",") != app.AuditCheckoutDenied {
		t.Errorf("audit = %v", a)
	}
}

// A checkout the organisation's policy denies never reaches mandate
// evaluation at all - its denial carries only the policy's reasons,
// each prefixed "policy_" so they can never be confused with a
// mandate-level reason of the same underlying name.
func TestSubmitCheckout_DeniedByOrgPolicy_MandateNeverEvaluated(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	f.submit.Policies = &apptest.FixedPolicy{
		Outcome: policy.Deny,
		Reasons: []policy.Reason{{Code: policy.ReasonOverTransactionLimit, Detail: "too much for the organisation"}},
	}

	cmd := f.laptop(m, "checkout_1")
	c := f.mustSubmit(t, cmd)
	if c.Status != commerce.CheckoutDenied {
		t.Fatalf("status = %s, want denied", c.Status)
	}
	if codesOf(c) != "policy_over_transaction_limit" {
		t.Errorf("denial codes = %s, want exactly the policy's own, prefixed", codesOf(c))
	}
	// The merchant has no permission to read the organisation's policy
	// itself - its exact limit ("too much for the organisation") must
	// never reach the checkout response as a side channel for it.
	if len(c.Denials) != 1 || c.Denials[0].Detail != "" {
		t.Errorf("denials = %+v, want the policy's detail left blank", c.Denials)
	}
	if a := f.actions("commerce.checkout."); strings.Join(a, ",") != app.AuditCheckoutDenied {
		t.Errorf("audit = %v", a)
	}
}

// Escalate is shimmed to behave exactly like Deny - commerce has no
// awaiting_review state or escalation workflow of its own, an
// accepted scope cut for Phase 6 (see docs/progress.md), not a gap
// a later phase will close.
func TestSubmitCheckout_EscalatedByOrgPolicy_TreatedAsDeniedForNow(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	f.submit.Policies = &apptest.FixedPolicy{
		Outcome: policy.Escalate,
		Reasons: []policy.Reason{{Code: policy.ReasonOverApprovalThreshold, Detail: "over the threshold"}},
	}

	c := f.mustSubmit(t, f.laptop(m, "checkout_1"))
	if c.Status != commerce.CheckoutDenied || codesOf(c) != "policy_over_approval_threshold" {
		t.Errorf("checkout = %s %s, want denied with the shimmed escalate reason", c.Status, codesOf(c))
	}
}

// When the organisation's policy allows, the mandate is evaluated
// exactly as before - an unrelated regression check.
func TestSubmitCheckout_AllowedByOrgPolicy_FallsThroughToMandateEvaluationUnchanged(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	policies := &apptest.FixedPolicy{Outcome: policy.Allow}
	f.submit.Policies = policies

	c := f.mustSubmit(t, f.laptop(m, "checkout_1"))
	if c.Status != commerce.CheckoutAuthorized || len(c.Denials) != 0 {
		t.Errorf("checkout = %+v, want authorized exactly as without a policy configured", c)
	}
	if len(policies.Calls) != 1 {
		t.Fatalf("policy evaluated %d times, want 1", len(policies.Calls))
	}
	in := policies.Calls[0]
	if in.Context != policy.ContextCommerce || in.Currency != "USD" || in.AmountMinor != 134600 {
		t.Errorf("evaluation input = %+v, want context=commerce matching the checkout's currency/total", in)
	}
	wantCategories := "electronics"
	if len(in.Categories) == 0 || strings.Join(in.Categories, ",") != wantCategories {
		t.Errorf("evaluation input categories = %v, want %q", in.Categories, wantCategories)
	}
}

// Authorized checkouts count at once: the budget and the purchase count.
func TestSubmitCheckout_AuthorizedCheckoutsUseUpTheMandate(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous) // 1,500 each, 3,000 total, 3 purchases
	if c := f.mustSubmit(t, priced(f.laptop(m, "c1"), 150000)); c.Status != commerce.CheckoutAuthorized {
		t.Fatalf("first: %s %s", c.Status, codesOf(c))
	}
	if c := f.mustSubmit(t, priced(f.laptop(m, "c2"), 150000)); c.Status != commerce.CheckoutAuthorized {
		t.Fatalf("second: %s %s", c.Status, codesOf(c))
	}
	if c := f.mustSubmit(t, priced(f.laptop(m, "c3"), 1)); c.Status != commerce.CheckoutDenied || codesOf(c) != "over_budget" {
		t.Errorf("third, the budget spent: %s %s", c.Status, codesOf(c))
	}
	// Cancelling releases what a checkout counted.
	if _, err := f.cancel.Run(context.Background(), shopA1, f.mustSubmit(t, priced(f.laptop(m, "c2"), 150000)).ID); err != nil {
		t.Fatal(err)
	}
	if c := f.mustSubmit(t, priced(f.laptop(m, "c4"), 100)); c.Status != commerce.CheckoutAuthorized {
		t.Errorf("after cancelling one: %s %s", c.Status, codesOf(c))
	}
	if c := f.mustSubmit(t, priced(f.laptop(m, "c5"), 100)); c.Status != commerce.CheckoutAuthorized {
		t.Errorf("third purchase: %s %s", c.Status, codesOf(c))
	}
	if c := f.mustSubmit(t, priced(f.laptop(m, "c6"), 100)); codesOf(c) != "over_transaction_count" {
		t.Errorf("fourth purchase: %s %s", c.Status, codesOf(c))
	}
}

// Concurrent checkouts can't overspend: they are decided one at a time.
func TestSubmitCheckout_ConcurrentCheckoutsCannotOverspend(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous) // 3,000 in total
	var wg sync.WaitGroup
	var mu sync.Mutex
	authorized := 0
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := f.submit.Run(context.Background(), shopA1, priced(f.laptop(m, fmt.Sprintf("race_%d", i)), 100000))
			if err == nil && c.Status == commerce.CheckoutAuthorized {
				mu.Lock()
				authorized++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if authorized != 3 {
		t.Errorf("%d of 10 checkouts of 1,000 authorized under a 3,000 budget, want 3", authorized)
	}
}

func TestSubmitCheckout_TheMandateMustBeActiveAndTheMerchants(t *testing.T) {
	f := newCheckoutsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	for _, other := range []commerceapp.MerchantPrincipal{shopA2, shopB} {
		if _, _, err := f.submit.Run(ctx, other, f.laptop(m, "x")); !errors.Is(err, commerceapp.ErrMandateUnknown) {
			t.Errorf("%s submitting under A1's mandate: %v", other.MerchantID, err)
		}
	}
	if _, _, err := f.submit.Run(ctx, shopA1, f.laptop(commerce.Mandate{ID: "mdt_nope"}, "y")); !errors.Is(err, commerceapp.ErrMandateUnknown) {
		t.Errorf("a made-up mandate: %v", err)
	}
	f.revoke.Run(ctx, shopA1, m.ID) // pending: grants nothing
	if c := f.mustSubmit(t, f.laptop(m, "z")); c.Status != commerce.CheckoutDenied || codesOf(c) != "mandate_not_active" {
		t.Errorf("revocation pending: %s %s", c.Status, codesOf(c))
	}
	var verr *app.ValidationError
	bad := f.laptop(m, "w")
	bad.TotalMinor++
	if _, _, err := f.submit.Run(ctx, shopA1, bad); !errors.As(err, &verr) || verr.Field != "total_minor" {
		t.Errorf("a total that doesn't add up: %v", err)
	}
	if _, _, err := f.submit.Run(ctx, shopA1, commerceapp.CheckoutCommand{ExternalID: "v"}); !errors.As(err, &verr) || verr.Field != "mandate_id" {
		t.Errorf("no mandate: %v", err)
	}
}

func TestSubmitCheckout_PerTransactionWaitsForTheCustomer(t *testing.T) {
	f := newCheckoutsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.PerTransaction)
	before := len(f.mailer.Sent())
	c := f.mustSubmit(t, f.laptop(m, "checkout_1"))
	if c.Status != commerce.CheckoutAwaitingApproval || c.ApprovalExpiresAt == nil || !c.ApprovalExpiresAt.Equal(f.clock.Now().Add(time.Hour)) {
		t.Fatalf("checkout = %+v", c)
	}
	sent := f.mailer.Sent()
	if len(sent) != before+1 || !strings.Contains(f.mailer.LastLinkTo(f.ada.Email), "/consent/checkout?token=") {
		t.Fatalf("approval email: %+v", sent[before:])
	}
	for _, want := range []string{"Lenovo ThinkPad", "USD 1346.00", "visa ending 4242"} {
		if !strings.Contains(sent[len(sent)-1].Text, want) {
			t.Errorf("the approval email does not show %q", want)
		}
	}
	token := f.token()
	v, err := f.viewCk.Run(ctx, token)
	if err != nil || v.ID != c.ID {
		t.Fatalf("view: %+v %v", v, err)
	}
	if _, err := f.decideCk.Run(ctx, token, "sha256:other", true); !errors.Is(err, commerceapp.ErrCheckoutMismatch) {
		t.Errorf("approving another checkout: %v", err)
	}
	got, err := f.decideCk.Run(ctx, token, c.Snapshot.Hash(), true)
	if err != nil || got.Status != commerce.CheckoutAuthorized {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if _, err := f.decideCk.Run(ctx, token, c.Snapshot.Hash(), true); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("the link twice: %v", err)
	}
	if a := f.actions("commerce.checkout."); strings.Join(a, ",") != app.AuditCheckoutAwaitingApproval+","+app.AuditCheckoutApproved {
		t.Errorf("audit = %v", a)
	}
}

// An awaiting checkout reserves nothing; limits are checked again when
// the customer approves.
func TestDecideCheckoutApproval_RechecksTheLimits(t *testing.T) {
	f := newCheckoutsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.PerTransaction)
	f.mustSubmit(t, priced(f.laptop(m, "a"), 150000))
	tokenA := f.token()
	b := f.mustSubmit(t, priced(f.laptop(m, "b"), 150000))
	tokenB := f.token()
	f.mustSubmit(t, priced(f.laptop(m, "c"), 150000))
	tokenC := f.token()
	f.decideCk.Run(ctx, tokenA, "", false) // rejected: counts nothing
	if got, err := f.decideCk.Run(ctx, tokenB, b.Snapshot.Hash(), true); err != nil || got.Status != commerce.CheckoutAuthorized {
		t.Fatalf("b: %+v %v", got, err)
	}
	c, _ := f.viewCk.Run(ctx, tokenC)
	f.mustSubmit(t, priced(f.laptop(m, "d"), 150000))
	tokenD := f.token()
	d, _ := f.viewCk.Run(ctx, tokenD)
	f.decideCk.Run(ctx, tokenC, c.Snapshot.Hash(), true)
	got, err := f.decideCk.Run(ctx, tokenD, d.Snapshot.Hash(), true)
	if err != nil || got.Status != commerce.CheckoutDenied || codesOf(got) != "over_budget" {
		t.Errorf("approving past the budget: %s %s %v", got.Status, codesOf(got), err)
	}
	if a := f.actions("commerce.checkout.rejected"); len(a) != 1 {
		t.Errorf("rejected events: %v", a)
	}
}

func TestDecideCheckoutApproval_ExpiresAfterAnHour(t *testing.T) {
	f := newCheckoutsFixture(t)
	m := f.mandate(t, "m1", commerce.PerTransaction)
	c := f.mustSubmit(t, f.laptop(m, "checkout_1"))
	token := f.token()
	f.clock.Advance(time.Hour)
	if _, err := f.decideCk.Run(context.Background(), token, c.Snapshot.Hash(), true); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("approving after an hour: %v", err)
	}
	if got, _ := f.getCk.Run(context.Background(), shopA1, c.ID); got.Status != commerce.CheckoutExpired {
		t.Errorf("status = %s, want expired", got.Status)
	}
	if _, err := f.cancel.Run(context.Background(), shopA1, c.ID); !errors.Is(err, commerce.ErrCheckoutNotCancelable) {
		t.Errorf("cancelling an expired checkout: %v", err)
	}
}

func TestCancelCheckout(t *testing.T) {
	f := newCheckoutsFixture(t)
	ctx := context.Background()
	auto := f.mandate(t, "m1", commerce.Autonomous)
	per := f.mandate(t, "m2", commerce.PerTransaction)
	a := f.mustSubmit(t, f.laptop(auto, "a"))
	w := f.mustSubmit(t, f.laptop(per, "w"))
	wToken := f.token()
	cmd := f.laptop(auto, "d")
	cmd.Currency = "GBP"
	d := f.mustSubmit(t, cmd)

	for _, id := range []commerce.CheckoutID{a.ID, w.ID} {
		got, err := f.cancel.Run(ctx, shopA1, id)
		if err != nil || got.Status != commerce.CheckoutCanceled || got.CanceledAt == nil {
			t.Errorf("cancel %s: %+v %v", id, got, err)
		}
		if again, err := f.cancel.Run(ctx, shopA1, id); err != nil || again.Status != commerce.CheckoutCanceled {
			t.Errorf("cancelling again: %+v %v", again, err)
		}
	}
	if _, err := f.decideCk.Run(ctx, wToken, w.Snapshot.Hash(), true); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("approving a cancelled checkout: %v", err)
	}
	if _, err := f.cancel.Run(ctx, shopA1, d.ID); !errors.Is(err, commerce.ErrCheckoutNotCancelable) {
		t.Errorf("cancelling a denied checkout: %v", err)
	}
	for _, other := range []commerceapp.MerchantPrincipal{shopA2, shopB} {
		if _, err := f.cancel.Run(ctx, other, d.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s cancelling: %v", other.MerchantID, err)
		}
		if _, err := f.getCk.Run(ctx, other, a.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s reading: %v", other.MerchantID, err)
		}
	}
}
