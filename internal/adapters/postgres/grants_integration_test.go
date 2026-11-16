//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// shiftedClock is the real clock moved by d: to look past a window.
type shiftedClock struct{ d time.Duration }

func (c shiftedClock) Now() time.Time { return realClock{}.Now().Add(c.d) }

func (w *checkoutWorld) redeemer(d time.Duration) *commerceapp.RedeemGrant {
	return &commerceapp.RedeemGrant{Checkouts: w.store, IDs: credentials.RandomIDs{}, Clock: shiftedClock{d}}
}

func (w *checkoutWorld) issuer(d time.Duration) *commerceapp.IssueGrant {
	return &commerceapp.IssueGrant{Checkouts: w.store, Secrets: credentials.RandomSecrets{}, Clock: shiftedClock{d}}
}

func (w *checkoutWorld) authorized(t *testing.T, m commerce.Mandate, ext string, total int64) commerceapp.CheckoutView {
	t.Helper()
	c, _, err := w.submit.Run(context.Background(), w.merchant, w.book(m, ext, total))
	if err != nil || c.Status != commerce.CheckoutAuthorized || c.Grant == "" {
		t.Fatalf("%s: %+v %v", ext, c, err)
	}
	return c
}

func TestGrantStore_RedeemOnceThenReturnTheSameExecution(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	c := w.authorized(t, m, "c1", 5000)
	redeem := w.redeemer(0)

	e, created, err := redeem.Run(ctx, w.merchant, c.Grant, "c1", c.Snapshot.Hash())
	if err != nil || !created || e.AmountMinor != 5000 || e.Currency != "EUR" || e.FundingMethod.ID != w.card.ID ||
		e.FundingMethod.Reference != "mfm_1" || e.Status != commerce.ExecutionPending {
		t.Fatalf("redeem: %+v %v %v", e, created, err)
	}
	if again, created, err := redeem.Run(ctx, w.merchant, c.Grant, "c1", c.Snapshot.Hash()); err != nil || created || again.ID != e.ID {
		t.Errorf("repeat: %+v %v %v", again, created, err)
	}
	got, _ := w.store.GetCheckout(ctx, w.merchant.Scope(), c.ID)
	if got.Status != commerce.CheckoutRedeemed || got.Authorization.ExecutionID != e.ID || got.Authorization.RedeemedAt == nil {
		t.Errorf("checkout after redemption: %+v %+v", got.Status, got.Authorization)
	}
	if x, err := w.store.GetExecution(ctx, w.merchant.Scope(), e.ID); err != nil || x.AuthorizationID != c.Authorization.ID {
		t.Errorf("get execution: %+v %v", x, err)
	}
	other, _ := w.u.newMerchantIn(t, w.merchant.OrgID)
	if _, _, err := redeem.Run(ctx, other, c.Grant, "c1", c.Snapshot.Hash()); !errors.Is(err, commerceapp.ErrGrantInvalid) {
		t.Errorf("a sibling storefront redeeming: %v", err)
	}
	if _, err := w.store.GetExecution(ctx, other.Scope(), e.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a sibling reading the execution: %v", err)
	}
	var stored int
	testPool(t).QueryRow(ctx, `SELECT count(*) FROM commerce_authorizations WHERE grant_hash = $1`, c.Grant).Scan(&stored)
	if stored != 0 {
		t.Error("the grant's secret is stored in plaintext")
	}
}

// Under the real mandate lock, 40 redemptions of one grant released at
// once make one execution, and every caller gets it.
func TestGrantStore_ConcurrentRedemptionsMakeOneExecution(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	c := w.authorized(t, m, "c1", 5000)
	redeem := w.redeemer(0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	made, ids := 0, map[commerce.ExecutionID]bool{}
	// Open the pool's connections first: dialling them one by one would
	// space the redemptions out and hide the race.
	var conns []*pgxpool.Conn
	for range int(w.store.pool.Config().MaxConns) {
		conn, err := w.store.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		conn.Release()
	}
	start := make(chan struct{})
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			e, created, err := redeem.Run(ctx, w.merchant, c.Grant, "c1", c.Snapshot.Hash())
			if err != nil {
				t.Errorf("redeem: %v", err)
				return
			}
			mu.Lock()
			ids[e.ID] = true
			if created {
				made++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	var n int
	testPool(t).QueryRow(ctx, `SELECT count(*) FROM commerce_executions WHERE checkout_id = $1`, string(c.ID)).Scan(&n)
	if made != 1 || len(ids) != 1 || n != 1 {
		t.Errorf("%d created, %d IDs, %d rows; want 1, 1, 1", made, len(ids), n)
	}
}

func TestGrantStore_ExpiryReissueCancelAndRevocation(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous) // 50 per purchase, 200 in total
	expiring := w.authorized(t, m, "c1", 5000)
	cancelled := w.authorized(t, m, "c2", 5000)
	open := w.authorized(t, m, "c3", 5000)
	w.authorized(t, m, "c4", 5000) // the budget is full

	// Past the window: the expiring grant fails, and stops counting.
	if _, _, err := w.redeemer(16*time.Minute).Run(ctx, w.merchant, expiring.Grant, "c1", expiring.Snapshot.Hash()); !errors.Is(err, commerce.ErrGrantExpired) {
		t.Errorf("redeeming after 16 minutes: %v", err)
	}
	later := &commerceapp.SubmitCheckout{Checkouts: w.store, Customers: w.submit.Customers, Emails: w.submit.Emails, IDs: credentials.RandomIDs{},
		Secrets: credentials.RandomSecrets{}, Clock: shiftedClock{16 * time.Minute}}
	if c, _, err := later.Run(ctx, w.merchant, w.book(m, "c5", 5000)); err != nil || c.Status != commerce.CheckoutAuthorized {
		t.Errorf("after the grants expired, the budget is free: %+v %v", c.Status, err)
	}

	// Reissue: the old secret stops, the expiry stays.
	re, err := w.issuer(time.Minute).Run(ctx, w.merchant, open.ID)
	if err != nil || re.Grant == open.Grant || !re.Authorization.ExpiresAt.Equal(open.Authorization.ExpiresAt) {
		t.Fatalf("reissue: %+v %v", re.Authorization, err)
	}
	if _, _, err := w.redeemer(0).Run(ctx, w.merchant, open.Grant, "c3", open.Snapshot.Hash()); !errors.Is(err, commerceapp.ErrGrantInvalid) {
		t.Errorf("the replaced grant: %v", err)
	}
	if _, err := w.issuer(16*time.Minute).Run(ctx, w.merchant, open.ID); !errors.Is(err, commerceapp.ErrGrantNotIssuable) {
		t.Errorf("reissuing after expiry: %v", err)
	}

	if _, err := w.cancel.Run(ctx, w.merchant, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.redeemer(0).Run(ctx, w.merchant, cancelled.Grant, "c2", cancelled.Snapshot.Hash()); !errors.Is(err, commerce.ErrGrantRevoked) {
		t.Errorf("a cancelled checkout's grant: %v", err)
	}

	// Confirming the mandate's revocation revokes the open grant, in the
	// same transaction.
	if _, err := w.revoke.Run(ctx, w.merchant, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.confirm.Run(ctx, w.token()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.redeemer(0).Run(ctx, w.merchant, re.Grant, "c3", open.Snapshot.Hash()); !errors.Is(err, commerce.ErrGrantRevoked) {
		t.Errorf("after the mandate's revocation: %v", err)
	}
	if got, _ := w.store.GetCheckout(ctx, w.merchant.Scope(), open.ID); got.Status != commerce.CheckoutRevoked {
		t.Errorf("open checkout after revocation: %s", got.Status)
	}
}

// A redeemed checkout can't be cancelled, and the schema keeps one
// execution per authorization.
func TestGrantStore_RedeemedIsFinal(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	c := w.authorized(t, m, "c1", 5000)
	e, _, err := w.redeemer(0).Run(ctx, w.merchant, c.Grant, "c1", c.Snapshot.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.cancel.Run(ctx, w.merchant, c.ID); !errors.Is(err, commerce.ErrCheckoutNotCancelable) {
		t.Errorf("cancelling a redeemed checkout: %v", err)
	}
	if _, err := testPool(t).Exec(ctx, `INSERT INTO commerce_executions SELECT 'exe_dup', authorization_id, checkout_id, external_checkout_id,
		checkout_hash, mandate_id, merchant_id, org_id, amount_minor, currency, funding_method_id, status, created_at
		FROM commerce_executions WHERE id = $1`, string(e.ID)); err == nil {
		t.Error("the schema allowed a second execution for one authorization")
	}
}
