package commerceapp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

type executionsFixture struct {
	*grantsFixture
	report    *commerceapp.ReportExecutionOutcome
	sweep     *commerceapp.SweepExecutions
	listExec  *commerceapp.ListExecutions
	listCk    *commerceapp.ListCheckouts
	dashboard *commerceapp.MerchantForDashboard
}

func newExecutionsFixture(t *testing.T) *executionsFixture {
	t.Helper()
	g := newGrantsFixture(t)
	store := g.submit.Checkouts
	g.get.Usage, g.list.Usage = store, store
	return &executionsFixture{
		grantsFixture: g,
		report:        &commerceapp.ReportExecutionOutcome{Checkouts: store, Clock: g.clock, Audit: g.auditor},
		sweep:         &commerceapp.SweepExecutions{Checkouts: store, Clock: g.clock, Audit: g.auditor},
		listExec:      &commerceapp.ListExecutions{Checkouts: store},
		listCk:        &commerceapp.ListCheckouts{Checkouts: store, Clock: g.clock},
	}
}

// redeemed submits an autonomous checkout of total under m and redeems it.
func (f *executionsFixture) redeemed(t *testing.T, m commerce.Mandate, ext string, total int64) commerce.Execution {
	t.Helper()
	c := f.authorized(t, m, ext, total)
	e, _, err := f.redeemC(c, c.Grant)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func outcome(src commerce.ReportSource, o commerce.ExecutionStatus, ref string, e commerce.Execution) commerce.ExecutionReport {
	return commerce.ExecutionReport{Source: src, Outcome: o, Provider: "stripe", ProviderPaymentReference: ref, AmountMinor: e.AmountMinor, Currency: e.Currency}
}

func (f *executionsFixture) mustReport(t *testing.T, e commerce.Execution, r commerce.ExecutionReport) commerceapp.ExecutionView {
	t.Helper()
	v, err := f.report.Run(context.Background(), shopA1, e.ID, r)
	if err != nil {
		t.Fatalf("report %s: %v", r.Outcome, err)
	}
	return v
}

func (f *executionsFixture) usage(t *testing.T, m commerce.Mandate) commerce.MandateUsage {
	t.Helper()
	v, err := f.get.Run(context.Background(), shopA1, m.ID)
	if err != nil || v.Usage == nil {
		t.Fatalf("mandate usage: %+v %v", v.Usage, err)
	}
	return *v.Usage
}

func TestReportExecution_SucceededStaysSpentAndIsFinal(t *testing.T) {
	f := newExecutionsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	e := f.redeemed(t, m, "c1", 100000)
	if e.IdempotencyKey() == "" || !e.ReportDeadline.Equal(f.clock.Now().Add(30*time.Minute)) {
		t.Errorf("execution = %+v", e)
	}
	v := f.mustReport(t, e, outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", e))
	if v.Result != commerce.ReportApplied || v.Status != commerce.ExecutionSucceeded || v.ProviderPaymentReference != "pi_1" {
		t.Fatalf("report: %+v", v)
	}
	if again := f.mustReport(t, e, outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", e)); again.Result != commerce.ReportDuplicate {
		t.Errorf("repeat: %s", again.Result)
	}
	if _, err := f.report.Run(context.Background(), shopA1, e.ID, outcome(commerce.ReportReconciliation, commerce.ExecutionFailed, "pi_1", e)); !errors.Is(err, commerce.ErrOutcomeConflict) {
		t.Errorf("contradiction: %v", err)
	}
	if c, _ := f.getCk.Run(context.Background(), shopA1, e.CheckoutID); c.Status != commerce.CheckoutPaid {
		t.Errorf("checkout = %s, want paid", c.Status)
	}
	if u := f.usage(t, m); u.PaidMinor != 100000 || u.SpentMinor != 100000 || u.Purchases != 1 {
		t.Errorf("usage = %+v", u)
	}
	d, err := f.exec.Run(context.Background(), shopA1, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range d.History {
		kinds = append(kinds, string(ev.Kind))
	}
	if strings.Join(kinds, ",") != "redeemed,reported,duplicate,refused" {
		t.Errorf("history = %v", kinds)
	}
	if a := f.actions("commerce.execution."); strings.Join(a, ",") != app.AuditExecutionSucceeded+","+app.AuditExecutionReportRefused {
		t.Errorf("audit = %v", a)
	}
}

func TestReportExecution_FailedIsReleased(t *testing.T) {
	f := newExecutionsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous) // 3,000 in total
	e := f.redeemed(t, m, "c1", 150000)
	f.redeemed(t, m, "c2", 150000)
	if c := f.mustSubmit(t, priced(f.laptop(m, "c3"), 100)); c.Status != commerce.CheckoutDenied {
		t.Fatal("budget should be full")
	}
	r := outcome(commerce.ReportDirect, commerce.ExecutionFailed, "", e)
	r.FailureReason = "card_declined"
	f.mustReport(t, e, r)
	if c := f.mustSubmit(t, priced(f.laptop(m, "c4"), 150000)); c.Status != commerce.CheckoutAuthorized {
		t.Errorf("after the failure was released: %s %s", c.Status, codesOf(c))
	}
	if c, _ := f.getCk.Run(context.Background(), shopA1, e.CheckoutID); c.Status != commerce.CheckoutPaymentFailed {
		t.Errorf("checkout = %s", c.Status)
	}
}

// Unknown is not failed: it keeps counting until reconciled.
func TestReportExecution_UnknownHoldsUntilReconciled(t *testing.T) {
	f := newExecutionsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	e := f.redeemed(t, m, "c1", 150000)
	f.mustReport(t, e, outcome(commerce.ReportDirect, commerce.ExecutionUnknown, "", e))
	if u := f.usage(t, m); u.SpentMinor != 150000 || u.PaidMinor != 0 {
		t.Errorf("unknown usage = %+v", u)
	}
	if _, err := f.report.Run(ctx, shopA1, e.ID, outcome(commerce.ReportDirect, commerce.ExecutionFailed, "", e)); !errors.Is(err, commerce.ErrReconciliationRequired) {
		t.Errorf("a direct report resolving unknown: %v", err)
	}
	page, err := f.listExec.Run(ctx, shopA1, commerceapp.ExecutionFilter{Status: commerce.ExecutionUnknown})
	if err != nil || len(page.Executions) != 1 || page.Executions[0].ID != e.ID {
		t.Fatalf("to reconcile: %+v %v", page, err)
	}
	v := f.mustReport(t, e, outcome(commerce.ReportReconciliation, commerce.ExecutionFailed, "pi_9", e))
	if v.Status != commerce.ExecutionFailed {
		t.Errorf("reconciled: %s", v.Status)
	}
	if u := f.usage(t, m); u.SpentMinor != 0 {
		t.Errorf("after reconciling a failure: %+v", u)
	}
	if a := f.actions("commerce.execution.reconciled"); len(a) != 1 {
		t.Errorf("audit = %v", a)
	}
}

func TestReportExecution_MismatchesAreRefusedAndRecorded(t *testing.T) {
	f := newExecutionsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	e := f.redeemed(t, m, "c1", 1000)
	r := outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", e)
	r.AmountMinor = 999
	if _, err := f.report.Run(context.Background(), shopA1, e.ID, r); !errors.Is(err, commerce.ErrReportMismatch) {
		t.Errorf("another amount: %v", err)
	}
	d, _ := f.exec.Run(context.Background(), shopA1, e.ID)
	if d.Status != commerce.ExecutionPending || d.History[len(d.History)-1].Kind != commerce.EventRefused || d.History[len(d.History)-1].AmountMinor != 999 {
		t.Errorf("after a refused report: %s %+v", d.Status, d.History)
	}
	var verr *app.ValidationError
	bad := outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "", e)
	if _, err := f.report.Run(context.Background(), shopA1, e.ID, bad); !errors.As(err, &verr) || verr.Field != "provider_payment_reference" {
		t.Errorf("succeeded without a reference: %v", err)
	}
	for _, other := range []commerceapp.MerchantPrincipal{shopA2, shopB} {
		if _, err := f.report.Run(context.Background(), other, e.ID, outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", e)); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s reporting: %v", other.MerchantID, err)
		}
	}
}

// A webhook event is applied once; the same ID with other content is
// a conflict.
func TestReportExecution_DuplicateWebhooks(t *testing.T) {
	f := newExecutionsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	e := f.redeemed(t, m, "c1", 1000)
	hook := outcome(commerce.ReportWebhook, commerce.ExecutionSucceeded, "pi_1", e)
	hook.EventID = "evt_1"
	if v := f.mustReport(t, e, hook); v.Result != commerce.ReportApplied {
		t.Fatalf("first: %s", v.Result)
	}
	for range 3 {
		if v := f.mustReport(t, e, hook); v.Result != commerce.ReportDuplicate {
			t.Errorf("redelivery: %s", v.Result)
		}
	}
	other := hook
	other.Outcome, other.ProviderPaymentReference = commerce.ExecutionFailed, "pi_1"
	if _, err := f.report.Run(context.Background(), shopA1, e.ID, other); !errors.Is(err, commerce.ErrEventConflict) {
		t.Errorf("same event ID, other content: %v", err)
	}
	e2 := f.redeemed(t, m, "c2", 1000)
	hook2 := outcome(commerce.ReportWebhook, commerce.ExecutionSucceeded, "pi_2", e2)
	hook2.EventID = "evt_1"
	if _, err := f.report.Run(context.Background(), shopA1, e2.ID, hook2); !errors.Is(err, commerce.ErrEventConflict) {
		t.Errorf("same event ID on another execution: %v", err)
	}
}

// A merchant that crashed mid-charge never reports: past the deadline
// the execution is presumed unknown, never failed.
func TestSweepExecutions_PendingPastTheDeadlineBecomesUnknown(t *testing.T) {
	f := newExecutionsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	stale := f.redeemed(t, m, "c1", 1000)
	done := f.redeemed(t, m, "c2", 1000)
	f.mustReport(t, done, outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_2", done))
	f.clock.Advance(29 * time.Minute)
	fresh := f.redeemed(t, m, "c3", 1000)
	f.clock.Advance(time.Minute)

	n, err := f.sweep.Run(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	for id, want := range map[commerce.ExecutionID]commerce.ExecutionStatus{stale.ID: commerce.ExecutionUnknown, done.ID: commerce.ExecutionSucceeded, fresh.ID: commerce.ExecutionPending} {
		if d, _ := f.exec.Run(ctx, shopA1, id); d.Status != want {
			t.Errorf("%s: %s, want %s", id, d.Status, want)
		}
	}
	if n, _ := f.sweep.Run(ctx); n != 0 {
		t.Errorf("sweeping again: %d", n)
	}
	d, _ := f.exec.Run(ctx, shopA1, stale.ID)
	if last := d.History[len(d.History)-1]; last.Kind != commerce.EventSwept || last.Source != commerce.ReportSweep {
		t.Errorf("history = %+v", d.History)
	}
	if u := f.usage(t, m); u.SpentMinor != 3000 {
		t.Errorf("unknown still counts: %+v", u)
	}
}

func TestListCheckouts(t *testing.T) {
	f := newExecutionsFixture(t)
	ctx := context.Background()
	m1 := f.mandate(t, "m1", commerce.Autonomous)
	m2 := f.mandate(t, "m2", commerce.Autonomous)
	a := f.authorized(t, m1, "a", 1000)
	f.clock.Advance(time.Second)
	b := f.authorized(t, m2, "b", 1000)
	f.clock.Advance(time.Second)
	cmd := priced(f.laptop(m1, "d"), 1000)
	cmd.Currency = "EUR"
	d := f.mustSubmit(t, cmd)

	all, err := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{})
	if err != nil || len(all.Checkouts) != 3 || all.Checkouts[0].ID != d.ID {
		t.Fatalf("all: %+v %v", all, err)
	}
	byMandate, _ := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{MandateID: m2.ID})
	denied, _ := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{Status: commerce.CheckoutDenied})
	byCustomer, _ := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{CustomerID: f.ada.ID})
	if len(byMandate.Checkouts) != 1 || byMandate.Checkouts[0].ID != b.ID || len(denied.Checkouts) != 1 || len(byCustomer.Checkouts) != 3 {
		t.Errorf("filters: %d %d %d", len(byMandate.Checkouts), len(denied.Checkouts), len(byCustomer.Checkouts))
	}
	f.clock.Advance(15 * time.Minute)
	expired, _ := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{Status: commerce.CheckoutExpired})
	if len(expired.Checkouts) != 2 {
		t.Errorf("expired (computed): %d", len(expired.Checkouts))
	}
	page, _ := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{Limit: 2})
	rest, _ := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{Limit: 2, Cursor: page.NextCursor})
	if len(page.Checkouts) != 2 || page.NextCursor == "" || len(rest.Checkouts) != 1 || rest.Checkouts[0].ID != a.ID {
		t.Errorf("paging: %d %q %d", len(page.Checkouts), page.NextCursor, len(rest.Checkouts))
	}
	if other, _ := f.listCk.Run(ctx, shopA2, commerceapp.CheckoutFilter{}); len(other.Checkouts) != 0 {
		t.Errorf("a sibling storefront sees %d", len(other.Checkouts))
	}
	var verr *app.ValidationError
	if _, err := f.listCk.Run(ctx, shopA1, commerceapp.CheckoutFilter{Status: "lost"}); !errors.As(err, &verr) {
		t.Errorf("bad status: %v", err)
	}
}

func TestMandateUsage(t *testing.T) {
	f := newExecutionsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous) // 1,500 each, 3,000 total, 3 purchases
	paid := f.redeemed(t, m, "c1", 100000)
	f.mustReport(t, paid, outcome(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", paid))
	f.authorized(t, m, "c2", 50000)
	u := f.usage(t, m)
	if u.Purchases != 2 || u.SpentMinor != 150000 || u.PaidMinor != 100000 {
		t.Errorf("usage = %+v", u)
	}
	page, _ := f.list.Run(context.Background(), shopA1, commerceapp.MandateFilter{})
	if len(page.Mandates) != 1 || page.Mandates[0].Usage == nil || page.Mandates[0].Usage.SpentMinor != 150000 {
		t.Errorf("listed usage: %+v", page.Mandates)
	}
}

func TestMerchantForDashboard(t *testing.T) {
	f := newExecutionsFixture(t)
	integrations := f.integrations
	f.dashboard = &commerceapp.MerchantForDashboard{Integrations: integrations}
	issued, err := (&commerceapp.CreateIntegration{Integrations: integrations, IDs: f.ids, Secrets: f.secrets, Clock: f.clock}).Run(context.Background(), orgA, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.dashboard.Run(context.Background(), orgA, issued.Integration.ID)
	if err != nil || p.MerchantID != issued.Integration.ID || p.OrgID != orgA {
		t.Errorf("principal = %+v %v", p, err)
	}
	if _, err := f.dashboard.Run(context.Background(), orgB, issued.Integration.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another organisation: %v", err)
	}
}
