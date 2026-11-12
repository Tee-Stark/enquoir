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

type checkoutWorld struct {
	*mandateWorld
	submit *commerceapp.SubmitCheckout
	cancel *commerceapp.CancelCheckout
	decide *commerceapp.DecideCheckoutApproval
	store  *CommerceCheckoutStore
}

func newCheckoutWorld(t *testing.T) *checkoutWorld {
	t.Helper()
	w := newMandateWorld(t)
	pool := testPool(t)
	clock, ids := realClock{}, credentials.RandomIDs{}
	audit := &app.Audit{Log: NewAuditLog(pool), IDs: ids, Clock: clock}
	store := NewCommerceCheckoutStore(pool)
	return &checkoutWorld{
		mandateWorld: w, store: store,
		submit: &commerceapp.SubmitCheckout{Checkouts: store, Customers: NewCommerceCustomerStore(pool), Emails: w.request.Emails, IDs: ids, Secrets: credentials.RandomSecrets{}, Clock: clock, Audit: audit},
		cancel: &commerceapp.CancelCheckout{Checkouts: store, Clock: clock, Audit: audit},
		decide: &commerceapp.DecideCheckoutApproval{Tokens: NewTokenStore(pool), Checkouts: store, IDs: ids, Clock: clock, Audit: audit},
	}
}

// mandate approves an EUR mandate: 50 per purchase, 200 in total, books.
func (w *checkoutWorld) mandate(t *testing.T, ext string, mode commerce.ApprovalMode) commerce.Mandate {
	t.Helper()
	c := w.cmd(ext)
	c.ApprovalMode = string(mode)
	r, _, err := w.request.Run(context.Background(), w.merchant, c)
	if err != nil {
		t.Fatal(err)
	}
	d, err := w.mandateWorld.decide.Run(context.Background(), w.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	return *d.Mandate
}

func (w *checkoutWorld) book(m commerce.Mandate, ext string, total int64) commerceapp.CheckoutCommand {
	return commerceapp.CheckoutCommand{ExternalID: ext, MandateID: m.ID, CustomerID: w.customer.ID, AgentID: w.agent.ID, FundingMethodID: w.card.ID,
		Currency: "EUR", Items: []commerce.CheckoutItem{{ProductID: "b1", Name: "A book", Category: "books", Quantity: 1, UnitPriceMinor: total}}, TotalMinor: total}
}

func TestCheckoutStore_DecidesAndCountsAgainstTheMandate(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)

	c, created, err := w.submit.Run(ctx, w.merchant, w.book(m, "c1", 5000))
	if err != nil || !created || c.Status != commerce.CheckoutAuthorized {
		t.Fatalf("submit: %+v %v %v", c, created, err)
	}
	again, created, err := w.submit.Run(ctx, w.merchant, w.book(m, "c1", 5000))
	if err != nil || created || again.ID != c.ID || again.Snapshot.Hash() != c.Snapshot.Hash() {
		t.Errorf("repeat: %+v %v %v", again, created, err)
	}
	if _, _, err := w.submit.Run(ctx, w.merchant, w.book(m, "c1", 4000)); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("changed: %v", err)
	}
	denied, _, _ := w.submit.Run(ctx, w.merchant, w.book(m, "c2", 6000))
	if denied.Status != commerce.CheckoutDenied || len(denied.Denials) != 1 || denied.Denials[0].Code != commerce.DenialOverTransactionLimit {
		t.Errorf("over the per-purchase limit: %+v", denied)
	}
	stored, _ := w.store.GetCheckout(ctx, w.merchant.Scope(), denied.ID)
	if len(stored.Denials) != 1 || stored.Denials[0].Detail == "" {
		t.Errorf("stored denials: %+v", stored.Denials)
	}
	for i := 3; i <= 5; i++ {
		w.submit.Run(ctx, w.merchant, w.book(m, fmt.Sprintf("c%d", i), 5000))
	}
	over, _, _ := w.submit.Run(ctx, w.merchant, w.book(m, "c6", 1))
	if over.Status != commerce.CheckoutDenied || over.Denials[0].Code != commerce.DenialOverBudget {
		t.Errorf("past the budget: %+v", over)
	}
	if got, err := w.cancel.Run(ctx, w.merchant, c.ID); err != nil || got.Status != commerce.CheckoutCanceled {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	if after, _, _ := w.submit.Run(ctx, w.merchant, w.book(m, "c7", 5000)); after.Status != commerce.CheckoutAuthorized {
		t.Errorf("after cancelling one: %+v", after)
	}
	if _, err := w.cancel.Run(ctx, w.merchant, denied.ID); !errors.Is(err, commerce.ErrCheckoutNotCancelable) {
		t.Errorf("cancelling a denied checkout: %v", err)
	}
}

// Under the real row lock, 12 concurrent checkouts of 50 against a 200
// budget: exactly 4 are authorized.
func TestCheckoutStore_ConcurrentCheckoutsCannotOverspend(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	var wg sync.WaitGroup
	var mu sync.Mutex
	authorized := 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := w.submit.Run(ctx, w.merchant, w.book(m, fmt.Sprintf("race_%d", i), 5000))
			if err != nil {
				t.Errorf("checkout %d: %v", i, err)
				return
			}
			if c.Status == commerce.CheckoutAuthorized {
				mu.Lock()
				authorized++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var spent int64
	testPool(t).QueryRow(ctx, `SELECT COALESCE(sum(total_minor), 0) FROM commerce_checkouts WHERE mandate_id = $1 AND status = 'authorized'`, string(m.ID)).Scan(&spent)
	if authorized != 4 || spent != 20000 {
		t.Errorf("%d authorized, %d spent; want 4 and 20000", authorized, spent)
	}
}

func TestCheckoutStore_CustomerApprovalRechecksAndSpendsTheLink(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.PerTransaction)
	a, _, _ := w.submit.Run(ctx, w.merchant, w.book(m, "a", 5000))
	if a.Status != commerce.CheckoutAwaitingApproval {
		t.Fatalf("submit: %+v", a)
	}
	token := w.token()
	if _, err := w.store.DecideCheckout(ctx, app.HashSecret("not-the-link"), a.ID, time.Now(), func(commerceapp.MandateRow, commerce.MandateUsage) commerceapp.CheckoutOutcome {
		return commerceapp.CheckoutOutcome{Status: commerce.CheckoutAuthorized}
	}); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("another token: %v", err)
	}
	got, err := w.decide.Run(ctx, token, a.Snapshot.Hash(), true)
	if err != nil || got.Status != commerce.CheckoutAuthorized {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if _, err := w.decide.Run(ctx, token, a.Snapshot.Hash(), true); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("the link twice: %v", err)
	}
	// Disable the card: the next approval is denied on the recheck.
	b, _, _ := w.submit.Run(ctx, w.merchant, w.book(m, "b", 5000))
	tokenB := w.token()
	w.fu.status.Run(ctx, w.merchant, w.card.ID, commerce.Disabled)
	got, err = w.decide.Run(ctx, tokenB, b.Snapshot.Hash(), true)
	if err != nil || got.Status != commerce.CheckoutDenied || got.Denials[0].Code != commerce.DenialMandateNotActive {
		t.Errorf("approving with the card disabled: %+v %v", got, err)
	}
}

func TestCheckoutStore_ScopedAndGuardedBySchema(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	c, _, _ := w.submit.Run(ctx, w.merchant, w.book(m, "c1", 5000))
	sibling, _ := w.u.newMerchantIn(t, w.merchant.OrgID)
	if _, _, err := w.submit.Run(ctx, sibling, w.book(m, "x", 100)); !errors.Is(err, commerceapp.ErrMandateUnknown) {
		t.Errorf("a sibling storefront using the mandate: %v", err)
	}
	if _, err := w.cancel.Run(ctx, sibling, c.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a sibling cancelling: %v", err)
	}
	if got, _ := w.store.GetCheckout(ctx, w.merchant.Scope(), c.ID); got.Status != commerce.CheckoutAuthorized {
		t.Errorf("after the sibling's attempt: %s", got.Status)
	}
	if _, err := testPool(t).Exec(ctx, `UPDATE commerce_checkouts SET snapshot = replace(snapshot, '5000', '1') WHERE id = $1`, string(c.ID)); err == nil {
		t.Error("a checkout's snapshot changed without its hash")
	}
	if _, err := testPool(t).Exec(ctx, `UPDATE commerce_checkouts SET merchant_id = $2 WHERE id = $1`, string(c.ID), string(sibling.MerchantID)); err == nil {
		t.Error("a checkout moved to a merchant whose mandate it isn't")
	}
}
