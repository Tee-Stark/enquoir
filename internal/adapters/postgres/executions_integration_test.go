//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

func (w *checkoutWorld) reporter(d time.Duration) *commerceapp.ReportExecutionOutcome {
	return &commerceapp.ReportExecutionOutcome{Checkouts: w.store, Clock: shiftedClock{d}}
}

func (w *checkoutWorld) redeemed(t *testing.T, m commerce.Mandate, ext string, total int64) commerce.Execution {
	t.Helper()
	c := w.authorized(t, m, ext, total)
	e, _, err := w.redeemer(0).Run(context.Background(), w.merchant, c.Grant, ext, c.Snapshot.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func rep(src commerce.ReportSource, o commerce.ExecutionStatus, ref string, e commerce.Execution) commerce.ExecutionReport {
	return commerce.ExecutionReport{Source: src, Outcome: o, Provider: "stripe", ProviderPaymentReference: ref, AmountMinor: e.AmountMinor, Currency: e.Currency}
}

func TestExecutionStore_ReportsSettleAndAreRecorded(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	paid := w.redeemed(t, m, "c1", 5000)
	failed := w.redeemed(t, m, "c2", 5000)
	held := w.redeemed(t, m, "c3", 5000)
	r := w.reporter(0)

	if v, err := r.Run(ctx, w.merchant, paid.ID, rep(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", paid)); err != nil || v.Status != commerce.ExecutionSucceeded {
		t.Fatalf("succeeded: %+v %v", v, err)
	}
	if _, err := r.Run(ctx, w.merchant, paid.ID, rep(commerce.ReportReconciliation, commerce.ExecutionFailed, "pi_1", paid)); !errors.Is(err, commerce.ErrOutcomeConflict) {
		t.Errorf("contradiction: %v", err)
	}
	r.Run(ctx, w.merchant, failed.ID, rep(commerce.ReportDirect, commerce.ExecutionFailed, "", failed))
	r.Run(ctx, w.merchant, held.ID, rep(commerce.ReportDirect, commerce.ExecutionUnknown, "", held))
	if _, err := r.Run(ctx, w.merchant, held.ID, rep(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_3", held)); !errors.Is(err, commerce.ErrReconciliationRequired) {
		t.Errorf("direct report resolving unknown: %v", err)
	}

	u, err := w.store.MandateUsage(ctx, w.merchant.Scope(), []commerce.MandateID{m.ID}, time.Now())
	if err != nil || u[m.ID] != (commerce.MandateUsage{Purchases: 2, SpentMinor: 10000, PaidMinor: 5000}) {
		t.Errorf("usage = %+v %v; want the paid one and the unknown one", u[m.ID], err)
	}
	for id, want := range map[commerce.CheckoutID]commerce.CheckoutStatus{paid.CheckoutID: commerce.CheckoutPaid, failed.CheckoutID: commerce.CheckoutPaymentFailed, held.CheckoutID: commerce.CheckoutRedeemed} {
		if c, _ := w.store.GetCheckout(ctx, w.merchant.Scope(), id); c.Status != want {
			t.Errorf("checkout %s: %s, want %s", id, c.Status, want)
		}
	}
	h, err := w.store.ExecutionHistory(ctx, w.merchant.Scope(), paid.ID)
	if err != nil || len(h) != 3 || h[0].Kind != commerce.EventRedeemed || h[1].Kind != commerce.EventReported || h[2].Kind != commerce.EventRefused || h[2].Detail == "" {
		t.Errorf("history = %+v %v", h, err)
	}
	if got, _ := w.store.GetExecution(ctx, w.merchant.Scope(), paid.ID); got.ProviderPaymentReference != "pi_1" || got.FinishedAt == nil {
		t.Errorf("stored execution: %+v", got)
	}
	if _, err := testPool(t).Exec(ctx, `DELETE FROM commerce_execution_events WHERE execution_id = $1`, string(paid.ID)); err == nil {
		t.Error("an execution's history was deleted")
	}
	other, _ := w.u.newMerchantIn(t, w.merchant.OrgID)
	if _, err := r.Run(ctx, other, held.ID, rep(commerce.ReportReconciliation, commerce.ExecutionFailed, "", held)); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a sibling reporting: %v", err)
	}
	page, _ := (&commerceapp.ListExecutions{Checkouts: w.store}).Run(ctx, w.merchant, commerceapp.ExecutionFilter{Status: commerce.ExecutionUnknown})
	if len(page.Executions) != 1 || page.Executions[0].ID != held.ID {
		t.Errorf("to reconcile: %+v", page.Executions)
	}
}

// The same webhook event delivered 12 times at once: applied once.
func TestExecutionStore_ConcurrentWebhookRedeliveries(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	e := w.redeemed(t, m, "c1", 5000)
	hook := rep(commerce.ReportWebhook, commerce.ExecutionSucceeded, "pi_1", e)
	hook.EventID = "evt_" + credentials.RandomIDs{}.NewID("x")
	r := w.reporter(0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[commerce.ReportResult]int{}
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := r.Run(ctx, w.merchant, e.ID, hook)
			if err != nil {
				t.Errorf("report: %v", err)
				return
			}
			mu.Lock()
			results[v.Result]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if results[commerce.ReportApplied] != 1 || results[commerce.ReportDuplicate] != 11 {
		t.Errorf("results = %v", results)
	}
	other := hook
	other.Outcome, other.ProviderPaymentReference = commerce.ExecutionFailed, ""
	if _, err := r.Run(ctx, w.merchant, e.ID, other); !errors.Is(err, commerce.ErrEventConflict) {
		t.Errorf("same event, other content: %v", err)
	}
}

func TestExecutionStore_SweepAndLists(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	stale := w.redeemed(t, m, "c1", 5000)
	reported := w.redeemed(t, m, "c2", 5000)
	w.reporter(0).Run(ctx, w.merchant, reported.ID, rep(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_2", reported))

	swept, err := w.store.SweepExecutions(ctx, time.Now().Add(31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range swept {
		if e.ID == stale.ID {
			found = e.Status == commerce.ExecutionUnknown
		}
		if e.ID == reported.ID {
			t.Error("a reported execution was swept")
		}
	}
	if !found {
		t.Errorf("the stale execution was not swept: %+v", swept)
	}
	h, _ := w.store.ExecutionHistory(ctx, w.merchant.Scope(), stale.ID)
	if h[len(h)-1].Kind != commerce.EventSwept {
		t.Errorf("history = %+v", h)
	}

	list := &commerceapp.ListCheckouts{Checkouts: w.store, Clock: shiftedClock{0}}
	w.authorized(t, m, "c3", 5000)
	cmd := w.book(m, "c4", 5000)
	cmd.Currency = "GBP"
	w.submit.Run(ctx, w.merchant, cmd)
	all, err := list.Run(ctx, w.merchant, commerceapp.CheckoutFilter{})
	if err != nil || len(all.Checkouts) != 4 || all.Checkouts[0].Snapshot.ExternalID != "c4" {
		t.Fatalf("all: %d %v", len(all.Checkouts), err)
	}
	for status, n := range map[commerce.CheckoutStatus]int{commerce.CheckoutDenied: 1, commerce.CheckoutPaid: 1, commerce.CheckoutRedeemed: 1, commerce.CheckoutAuthorized: 1} {
		if p, _ := list.Run(ctx, w.merchant, commerceapp.CheckoutFilter{Status: status}); len(p.Checkouts) != n {
			t.Errorf("%s: %d, want %d", status, len(p.Checkouts), n)
		}
	}
	later := &commerceapp.ListCheckouts{Checkouts: w.store, Clock: shiftedClock{16 * time.Minute}}
	if p, _ := later.Run(ctx, w.merchant, commerceapp.CheckoutFilter{Status: commerce.CheckoutExpired}); len(p.Checkouts) != 1 {
		t.Errorf("expired, computed: %d", len(p.Checkouts))
	}
	p1, _ := list.Run(ctx, w.merchant, commerceapp.CheckoutFilter{Limit: 3})
	p2, _ := list.Run(ctx, w.merchant, commerceapp.CheckoutFilter{Limit: 3, Cursor: p1.NextCursor})
	if len(p1.Checkouts) != 3 || len(p2.Checkouts) != 1 || p2.NextCursor != "" {
		t.Errorf("paging: %d %d", len(p1.Checkouts), len(p2.Checkouts))
	}
	if p, _ := list.Run(ctx, w.merchant, commerceapp.CheckoutFilter{MandateID: "mdt_other", CustomerID: w.customer.ID}); len(p.Checkouts) != 0 {
		t.Errorf("another mandate: %d", len(p.Checkouts))
	}
	_ = fmt.Sprint
}
