//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

type orgActivityWorld struct {
	*checkoutWorld
	listOrgCk   *commerceapp.ListOrgCheckouts
	listOrgExec *commerceapp.ListOrgExecutions
	getOrgCk    *commerceapp.GetOrgCheckout
	getOrgExec  *commerceapp.GetOrgExecution
}

func newOrgActivityWorld(t *testing.T) *orgActivityWorld {
	t.Helper()
	w := newCheckoutWorld(t)
	clock := realClock{}
	return &orgActivityWorld{
		checkoutWorld: w,
		listOrgCk:     &commerceapp.ListOrgCheckouts{Reader: w.store, Clock: clock},
		listOrgExec:   &commerceapp.ListOrgExecutions{Reader: w.store},
		getOrgCk:      &commerceapp.GetOrgCheckout{Reader: w.store, Clock: clock},
		getOrgExec:    &commerceapp.GetOrgExecution{Reader: w.store},
	}
}

// secondStorefront registers a second merchant integration of w's own
// organisation, with its own customer/agent/funding method/mandate -
// commerce resources are scoped per merchant, never shared within one
// organisation, even for an org-wide read.
func (w *orgActivityWorld) secondStorefront(t *testing.T) (commerceapp.MerchantPrincipal, commerce.Mandate, commerce.Customer, commerce.Agent, commerce.FundingMethod) {
	t.Helper()
	ctx := context.Background()
	ids := credentials.RandomIDs{}
	merchant, _ := w.u.newMerchantIn(t, w.merchant.OrgID)
	addr := "delivered+" + ids.NewID("c2") + "@resend.dev"
	cust, _, err := w.u.registerCustomer.Run(ctx, merchant, "cust2", addr)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := w.u.registerAgent.Run(ctx, merchant, "assistant2", "Assistant 2")
	if err != nil {
		t.Fatal(err)
	}
	card, _, err := w.fu.register.Run(ctx, merchant, cardOf(cust.ID, "mfm_2"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewCommerceMandateStore(testPool(t))
	request := &commerceapp.RequestMandate{Mandates: store, Customers: NewCommerceCustomerStore(testPool(t)), Agents: NewCommerceAgentStore(testPool(t)),
		Methods: NewCommerceFundingMethodStore(testPool(t)), Emails: w.request.Emails, IDs: ids, Clock: realClock{}, Audit: w.submit.Audit}
	r, _, err := request.Run(ctx, merchant, commerceapp.MandateRequestCommand{
		ExternalID: "m2", CustomerID: cust.ID, AgentID: agent.ID, FundingMethodID: card.ID, ApprovalMode: "autonomous",
		Currency: "EUR", MaxPerTransactionMinor: 5000, MaxTotalMinor: 20000, AllowedCategories: []string{"books"}, ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	decide := &commerceapp.DecideConsent{Tokens: NewTokenStore(testPool(t)), Mandates: store, IDs: ids, Clock: realClock{}, Audit: w.submit.Audit}
	d, err := decide.Run(ctx, w.mailer.TokenTo(cust.Email), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	return merchant, *d.Mandate, cust, agent, card
}

func (w *orgActivityWorld) checkoutOn(t *testing.T, merchant commerceapp.MerchantPrincipal, m commerce.Mandate, cust commerce.Customer, agent commerce.Agent, card commerce.FundingMethod, ext string, total int64) commerceapp.CheckoutView {
	t.Helper()
	submit := &commerceapp.SubmitCheckout{Checkouts: w.store, Customers: NewCommerceCustomerStore(testPool(t)), Emails: w.submit.Emails,
		IDs: credentials.RandomIDs{}, Secrets: credentials.RandomSecrets{}, Clock: realClock{}, Audit: w.submit.Audit}
	v, _, err := submit.Run(context.Background(), merchant, commerceapp.CheckoutCommand{
		ExternalID: ext, MandateID: m.ID, CustomerID: cust.ID, AgentID: agent.ID, FundingMethodID: card.ID,
		Currency: "EUR", Items: []commerce.CheckoutItem{{ProductID: "b1", Name: "A book", Category: "books", Quantity: 1, UnitPriceMinor: total}}, TotalMinor: total,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOrgActivity_SpansMerchantsWithinOneOrgNeverAcrossOrgs(t *testing.T) {
	ctx := context.Background()
	w := newOrgActivityWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	a, _, err := w.submit.Run(ctx, w.merchant, w.book(m, "a", 5000))
	if err != nil {
		t.Fatal(err)
	}
	merchant2, m2, cust2, agent2, card2 := w.secondStorefront(t)
	v2 := w.checkoutOn(t, merchant2, m2, cust2, agent2, card2, "b", 5000)

	all, err := w.listOrgCk.Run(ctx, w.merchant.OrgID, commerceapp.OrgCheckoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[commerce.CheckoutID]bool{}
	for _, c := range all.Checkouts {
		seen[c.ID] = true
	}
	if !seen[a.ID] || !seen[v2.ID] {
		t.Fatalf("org-wide list missing a merchant's checkout: a=%v v2=%v seen=%+v", a.ID, v2.ID, seen)
	}

	otherOrg, _ := w.u.newMerchant(t, NewAccountStore(testPool(t)))
	noneForOther, err := w.listOrgCk.Run(ctx, otherOrg.OrgID, commerceapp.OrgCheckoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range noneForOther.Checkouts {
		if c.ID == a.ID || c.ID == v2.ID {
			t.Errorf("a sibling organisation saw %s", c.ID)
		}
	}
}

func TestGetOrgCheckoutAndExecution_ScopedByOrgNotMerchant(t *testing.T) {
	ctx := context.Background()
	w := newOrgActivityWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	e := w.redeemed(t, m, "a", 5000)

	ck, err := w.getOrgCk.Run(ctx, w.merchant.OrgID, e.CheckoutID)
	if err != nil || ck.ID != e.CheckoutID {
		t.Fatalf("get checkout: %+v %v", ck, err)
	}
	otherOrg, _ := w.u.newMerchant(t, NewAccountStore(testPool(t)))
	if _, err := w.getOrgCk.Run(ctx, otherOrg.OrgID, e.CheckoutID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another org's checkout: %v", err)
	}

	d, err := w.getOrgExec.Run(ctx, w.merchant.OrgID, e.ID)
	if err != nil || d.ID != e.ID || len(d.History) != 1 {
		t.Fatalf("get execution: %+v %v", d, err)
	}
	if _, err := w.getOrgExec.Run(ctx, otherOrg.OrgID, e.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another org's execution: %v", err)
	}
}

func TestListOrgExecutions_AgentCurrencyAndDateFilters(t *testing.T) {
	ctx := context.Background()
	w := newOrgActivityWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	e := w.redeemed(t, m, "a", 5000)

	byAgent, err := w.listOrgExec.Run(ctx, w.merchant.OrgID, commerceapp.OrgExecutionFilter{AgentID: w.agent.ID})
	if err != nil || len(byAgent.Executions) != 1 || byAgent.Executions[0].ID != e.ID {
		t.Fatalf("by agent: %+v %v", byAgent, err)
	}
	byCurrency, err := w.listOrgExec.Run(ctx, w.merchant.OrgID, commerceapp.OrgExecutionFilter{Currency: "EUR"})
	if err != nil || len(byCurrency.Executions) != 1 {
		t.Fatalf("by currency: %+v %v", byCurrency, err)
	}
	future, err := w.listOrgExec.Run(ctx, w.merchant.OrgID, commerceapp.OrgExecutionFilter{From: time.Now().Add(time.Hour)})
	if err != nil || len(future.Executions) != 0 {
		t.Fatalf("future from: %+v %v", future, err)
	}
	wrongAgent, err := w.listOrgExec.Run(ctx, w.merchant.OrgID, commerceapp.OrgExecutionFilter{AgentID: "cagt_nope"})
	if err != nil || len(wrongAgent.Executions) != 0 {
		t.Fatalf("wrong agent: %+v %v", wrongAgent, err)
	}
}
