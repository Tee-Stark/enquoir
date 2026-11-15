package commerceapp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

type orgActivityFixture struct {
	*executionsFixture
	listOrgCk   *commerceapp.ListOrgCheckouts
	listOrgExec *commerceapp.ListOrgExecutions
	getOrgCk    *commerceapp.GetOrgCheckout
	getOrgExec  *commerceapp.GetOrgExecution
}

func newOrgActivityFixture(t *testing.T) *orgActivityFixture {
	t.Helper()
	f := newExecutionsFixture(t)
	reader := f.submit.Checkouts.(commerceapp.CommerceOrgReader)
	return &orgActivityFixture{
		executionsFixture: f,
		listOrgCk:         &commerceapp.ListOrgCheckouts{Reader: reader, Clock: f.clock},
		listOrgExec:       &commerceapp.ListOrgExecutions{Reader: reader},
		getOrgCk:          &commerceapp.GetOrgCheckout{Reader: reader, Clock: f.clock},
		getOrgExec:        &commerceapp.GetOrgExecution{Reader: reader},
	}
}

// Two storefronts of org A (shopA1, shopA2) and one of org B: an
// org-wide list sees across the first two, never the third.
func TestListOrgCheckouts_SpansMerchantsWithinOneOrg(t *testing.T) {
	f := newOrgActivityFixture(t)
	ctx := context.Background()
	m1 := f.mandate(t, "m1", commerce.Autonomous)
	a := f.authorized(t, m1, "a", 1000)
	f.clock.Advance(time.Second)

	// shopA2 is a second storefront of org A, with its own customer,
	// agent, funding method and mandate - commerce resources are scoped
	// per merchant, never shared even within one organisation.
	ada2 := f.customer(t, shopA2, "cust_a2", "ada2@example.com")
	bot2 := f.agent(t, shopA2, "assistant", "A2 Shopping Assistant")
	card2 := f.bind(t, shopA2, visa(ada2.ID, "mfm_a2"))
	c2 := f.cmd("m2")
	c2.CustomerID, c2.AgentID, c2.FundingMethodID = ada2.ID, bot2.ID, card2.ID
	r2, _, err := f.request.Run(ctx, shopA2, c2)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := f.decide.Run(ctx, f.mailer.TokenTo(ada2.Email), r2.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	m2 := *d2.Mandate
	cmd2 := commerceapp.CheckoutCommand{
		ExternalID: "b", MandateID: m2.ID, CustomerID: ada2.ID, AgentID: bot2.ID, FundingMethodID: card2.ID, Currency: "USD",
		Items:      []commerce.CheckoutItem{{ProductID: "laptop", Name: "Laptop", Category: "electronics", Quantity: 1, UnitPriceMinor: 1000}},
		TotalMinor: 1000,
	}
	v2, _, err := (&commerceapp.SubmitCheckout{Checkouts: f.submit.Checkouts, Customers: f.submit.Customers, Emails: f.submit.Emails,
		IDs: f.submit.IDs, Secrets: f.submit.Secrets, Clock: f.submit.Clock, Audit: f.submit.Audit}).Run(ctx, shopA2, cmd2)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)

	all, err := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Checkouts) != 2 || all.Checkouts[0].ID != v2.ID || all.Checkouts[1].ID != a.ID {
		t.Fatalf("org-wide list = %+v", all)
	}
	other, err := f.listOrgCk.Run(ctx, orgB, commerceapp.OrgCheckoutFilter{})
	if err != nil || len(other.Checkouts) != 0 {
		t.Errorf("org B sees %d, %v", len(other.Checkouts), err)
	}
}

func TestListOrgCheckouts_FiltersAndPages(t *testing.T) {
	f := newOrgActivityFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	a := f.authorized(t, m, "a", 1000)
	f.clock.Advance(time.Second)
	f.authorized(t, m, "b", 1000)
	f.clock.Advance(time.Second)
	cmd := priced(f.laptop(m, "c"), 1000)
	cmd.Currency = "EUR"
	eur := f.mustSubmit(t, cmd)

	byCurrency, err := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{Currency: "EUR"})
	if err != nil || len(byCurrency.Checkouts) != 1 || byCurrency.Checkouts[0].ID != eur.ID {
		t.Fatalf("by currency: %+v %v", byCurrency, err)
	}
	byAgent, err := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{AgentID: f.bot.ID})
	if err != nil || len(byAgent.Checkouts) != 3 {
		t.Fatalf("by agent: %+v %v", byAgent, err)
	}
	page, _ := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{Limit: 2})
	rest, _ := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{Limit: 2, Cursor: page.NextCursor})
	if len(page.Checkouts) != 2 || page.NextCursor == "" || len(rest.Checkouts) != 1 || rest.Checkouts[0].ID != a.ID {
		t.Errorf("paging: %d %q %d", len(page.Checkouts), page.NextCursor, len(rest.Checkouts))
	}

	var verr *app.ValidationError
	if _, err := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{Status: "lost"}); !errors.As(err, &verr) || verr.Field != "status" {
		t.Errorf("bad status: %v", err)
	}
	if _, err := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{Currency: "XYZ"}); !errors.As(err, &verr) || verr.Field != "currency" {
		t.Errorf("bad currency: %v", err)
	}
	later := f.clock.Now()
	if _, err := f.listOrgCk.Run(ctx, orgA, commerceapp.OrgCheckoutFilter{From: later, To: later.Add(-time.Hour)}); !errors.As(err, &verr) || verr.Field != "from" {
		t.Errorf("backwards range: %v", err)
	}
}

func TestListOrgExecutions_SpansMerchantsAndFilters(t *testing.T) {
	f := newOrgActivityFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	e1 := f.redeemed(t, m, "a", 1000)
	f.clock.Advance(time.Second)
	e2 := f.redeemed(t, m, "b", 1000)
	f.mustReport(t, e2, outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", e2))

	all, err := f.listOrgExec.Run(ctx, orgA, commerceapp.OrgExecutionFilter{})
	if err != nil || len(all.Executions) != 2 {
		t.Fatalf("all: %+v %v", all, err)
	}
	pending, err := f.listOrgExec.Run(ctx, orgA, commerceapp.OrgExecutionFilter{Status: commerce.ExecutionPending})
	if err != nil || len(pending.Executions) != 1 || pending.Executions[0].ID != e1.ID {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	byAgent, err := f.listOrgExec.Run(ctx, orgA, commerceapp.OrgExecutionFilter{AgentID: f.bot.ID})
	if err != nil || len(byAgent.Executions) != 2 {
		t.Fatalf("by agent: %+v %v", byAgent, err)
	}
	other, err := f.listOrgExec.Run(ctx, orgB, commerceapp.OrgExecutionFilter{})
	if err != nil || len(other.Executions) != 0 {
		t.Errorf("org B sees %d, %v", len(other.Executions), err)
	}
	var verr *app.ValidationError
	if _, err := f.listOrgExec.Run(ctx, orgA, commerceapp.OrgExecutionFilter{Status: "lost"}); !errors.As(err, &verr) {
		t.Errorf("bad status: %v", err)
	}
}

func TestGetOrgCheckout_AndGetOrgExecution(t *testing.T) {
	f := newOrgActivityFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	e := f.redeemed(t, m, "a", 1000)

	ck, err := f.getOrgCk.Run(ctx, orgA, e.CheckoutID)
	if err != nil || ck.ID != e.CheckoutID {
		t.Fatalf("get checkout: %+v %v", ck, err)
	}
	if _, err := f.getOrgCk.Run(ctx, orgB, e.CheckoutID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another org's checkout: %v", err)
	}

	d, err := f.getOrgExec.Run(ctx, orgA, e.ID)
	if err != nil || d.ID != e.ID || len(d.History) != 1 || d.History[0].Kind != commerce.EventRedeemed {
		t.Fatalf("get execution: %+v %v", d, err)
	}
	if _, err := f.getOrgExec.Run(ctx, orgB, e.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another org's execution: %v", err)
	}
}
