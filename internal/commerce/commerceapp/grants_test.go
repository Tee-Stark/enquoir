package commerceapp_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

type grantsFixture struct {
	*checkoutsFixture
	issue  *commerceapp.IssueGrant
	redeem *commerceapp.RedeemGrant
	exec   *commerceapp.GetExecution
}

func newGrantsFixture(t *testing.T) *grantsFixture {
	t.Helper()
	f := newCheckoutsFixture(t)
	store := f.submit.Checkouts
	return &grantsFixture{
		checkoutsFixture: f,
		issue:            &commerceapp.IssueGrant{Checkouts: store, Secrets: f.secrets, Clock: f.clock, Audit: f.auditor},
		redeem:           &commerceapp.RedeemGrant{Checkouts: store, IDs: f.ids, Clock: f.clock, Audit: f.auditor},
		exec:             &commerceapp.GetExecution{Checkouts: store},
	}
}

// authorized submits an autonomous checkout of total and returns it with
// its grant.
func (f *grantsFixture) authorized(t *testing.T, m commerce.Mandate, ext string, total int64) commerceapp.CheckoutView {
	t.Helper()
	c := f.mustSubmit(t, priced(f.laptop(m, ext), total))
	if c.Status != commerce.CheckoutAuthorized || c.Grant == "" {
		t.Fatalf("%s: %s, grant %q", ext, c.Status, c.Grant)
	}
	return c
}

func (f *grantsFixture) redeemC(c commerceapp.CheckoutView, grant string) (commerce.Execution, bool, error) {
	return f.redeem.Run(context.Background(), shopA1, grant, string(c.Snapshot.ExternalID), c.Snapshot.Hash())
}

func TestSubmitCheckout_AnAutonomousAuthorizationCarriesItsGrantOnce(t *testing.T) {
	f := newGrantsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	c := f.authorized(t, m, "c1", 1000)
	a := c.Authorization
	if !strings.HasPrefix(c.Grant, "enq_gr_") || a == nil || !a.ExpiresAt.Equal(f.clock.Now().Add(15*time.Minute)) ||
		a.GrantHash != app.HashSecret(c.Grant) || a.AmountMinor != 1000 || a.CheckoutHash != c.Snapshot.Hash() {
		t.Fatalf("authorization = %+v, grant %q", a, c.Grant)
	}
	again, _, _ := f.submit.Run(context.Background(), shopA1, priced(f.laptop(m, "c1"), 1000))
	if again.Grant != "" || again.Authorization == nil {
		t.Errorf("a repeat shows the grant again: %q", again.Grant)
	}
	if denied := f.mustSubmit(t, priced(f.laptop(m, "c2"), 999999)); denied.Grant != "" || denied.Authorization != nil {
		t.Errorf("a denied checkout has a grant: %+v", denied)
	}
}

func TestRedeemGrant_CreatesOneExecutionAndRepeatsReturnIt(t *testing.T) {
	f := newGrantsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	c := f.authorized(t, m, "c1", 134600)
	ctx := app.WithActor(context.Background(), app.Actor{MerchantID: string(shopA1.MerchantID)})

	e, created, err := f.redeem.Run(ctx, shopA1, c.Grant, "c1", c.Snapshot.Hash())
	if err != nil || !created {
		t.Fatalf("redeem: %v %v", created, err)
	}
	if e.AmountMinor != 134600 || e.Currency != "USD" || e.FundingMethod.ID != f.card.ID || e.FundingMethod.Reference != "mfm_1" ||
		e.Status != commerce.ExecutionPending || e.CheckoutID != c.ID || e.AuthorizationID != c.Authorization.ID {
		t.Errorf("execution = %+v", e)
	}
	for range 3 {
		again, created, err := f.redeem.Run(ctx, shopA1, c.Grant, "c1", c.Snapshot.Hash())
		if err != nil || created || again.ID != e.ID {
			t.Errorf("repeat: %+v %v %v", again, created, err)
		}
	}
	// Even after the grant's window: a repeat is a lookup, not a charge.
	f.clock.Advance(time.Hour)
	if again, created, err := f.redeem.Run(ctx, shopA1, c.Grant, "c1", c.Snapshot.Hash()); err != nil || created || again.ID != e.ID {
		t.Errorf("repeat after the window: %+v %v %v", again, created, err)
	}
	if _, _, err := f.redeem.Run(ctx, shopA1, c.Grant, "other", c.Snapshot.Hash()); !errors.Is(err, commerce.ErrRedemptionMismatch) {
		t.Errorf("a repeat naming another checkout: %v", err)
	}
	got, _ := f.getCk.Run(ctx, shopA1, c.ID)
	if got.Status != commerce.CheckoutRedeemed || got.Authorization.ExecutionID != e.ID {
		t.Errorf("checkout after redemption: %s %+v", got.Status, got.Authorization)
	}
	if x, err := f.exec.Run(ctx, shopA1, e.ID); err != nil || x.ID != e.ID {
		t.Errorf("get execution: %+v %v", x, err)
	}
	if a := f.actions("commerce.authorization.redeemed"); len(a) != 1 {
		t.Errorf("audit = %v, want one redemption", a)
	}
}

func TestRedeemGrant_Checks(t *testing.T) {
	f := newGrantsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	c := f.authorized(t, m, "c1", 1000)

	for name, tc := range map[string]struct {
		grant, ext, hash string
		p                commerceapp.MerchantPrincipal
		want             error
	}{
		"another checkout":       {c.Grant, "c9", c.Snapshot.Hash(), shopA1, commerce.ErrRedemptionMismatch},
		"another version":        {c.Grant, "c1", "sha256:00", shopA1, commerce.ErrRedemptionMismatch},
		"a made-up grant":        {"enq_gr_made_up", "c1", c.Snapshot.Hash(), shopA1, commerceapp.ErrGrantInvalid},
		"not a grant":            {"enq_mk_secret0001", "c1", c.Snapshot.Hash(), shopA1, commerceapp.ErrGrantInvalid},
		"a sibling storefront's": {c.Grant, "c1", c.Snapshot.Hash(), shopA2, commerceapp.ErrGrantInvalid},
		"another organisation's": {c.Grant, "c1", c.Snapshot.Hash(), shopB, commerceapp.ErrGrantInvalid},
	} {
		if _, _, err := f.redeem.Run(ctx, tc.p, tc.grant, tc.ext, tc.hash); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}

	// Paused mandate: no redemption, and the grant stays redeemable after.
	f.status.Run(ctx, shopA1, f.card.ID, commerce.Disabled)
	if _, _, err := f.redeemC(c, c.Grant); !errors.Is(err, commerceapp.ErrMandateNotActive) {
		t.Errorf("card disabled: %v", err)
	}
	f.status.Run(ctx, shopA1, f.card.ID, commerce.Active)
	f.revoke.Run(ctx, shopA1, m.ID)
	if _, _, err := f.redeemC(c, c.Grant); !errors.Is(err, commerceapp.ErrMandateNotActive) {
		t.Errorf("revocation pending: %v", err)
	}

	f.clock.Advance(15 * time.Minute)
	if _, _, err := f.redeemC(c, c.Grant); !errors.Is(err, commerce.ErrGrantExpired) {
		t.Errorf("after 15 minutes: %v", err)
	}
	if got, _ := f.getCk.Run(ctx, shopA1, c.ID); got.Status != commerce.CheckoutExpired {
		t.Errorf("checkout with an expired grant: %s", got.Status)
	}
}

// An unredeemed grant that expires stops counting against the mandate.
func TestAnExpiredGrantReleasesTheBudget(t *testing.T) {
	f := newGrantsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous) // 3,000 in total
	f.authorized(t, m, "c1", 150000)
	kept := f.authorized(t, m, "c2", 150000)
	if _, _, err := f.redeemC(kept, kept.Grant); err != nil {
		t.Fatal(err)
	}
	if c := f.mustSubmit(t, priced(f.laptop(m, "c3"), 100)); c.Status != commerce.CheckoutDenied {
		t.Fatalf("budget full: %s", c.Status)
	}
	f.clock.Advance(15 * time.Minute)
	if c := f.mustSubmit(t, priced(f.laptop(m, "c4"), 150000)); c.Status != commerce.CheckoutAuthorized {
		t.Errorf("after c1's grant expired: %s %s", c.Status, codesOf(c))
	}
	// The redeemed one still counts: nothing more fits.
	if c := f.mustSubmit(t, priced(f.laptop(m, "c5"), 100)); c.Status != commerce.CheckoutDenied {
		t.Errorf("a redeemed checkout stopped counting: %s", c.Status)
	}
}

func TestIssueGrant_ReplacesTheSecretWithoutExtendingIt(t *testing.T) {
	f := newGrantsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	c := f.authorized(t, m, "c1", 1000)
	f.clock.Advance(10 * time.Minute)

	re, err := f.issue.Run(ctx, shopA1, c.ID)
	if err != nil || re.Grant == "" || re.Grant == c.Grant || !re.Authorization.ExpiresAt.Equal(c.Authorization.ExpiresAt) {
		t.Fatalf("reissue: %+v %v", re, err)
	}
	if _, _, err := f.redeemC(c, c.Grant); !errors.Is(err, commerceapp.ErrGrantInvalid) {
		t.Errorf("the replaced grant: %v", err)
	}
	if _, _, err := f.redeemC(c, re.Grant); err != nil {
		t.Errorf("the new grant: %v", err)
	}
	if _, err := f.issue.Run(ctx, shopA1, c.ID); !errors.Is(err, commerce.ErrGrantRedeemed) {
		t.Errorf("reissuing a redeemed grant: %v", err)
	}
	if _, err := f.issue.Run(ctx, shopA2, c.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another merchant reissuing: %v", err)
	}
	other := f.authorized(t, m, "c2", 1000)
	f.clock.Advance(15 * time.Minute)
	if _, err := f.issue.Run(ctx, shopA1, other.ID); !errors.Is(err, commerceapp.ErrGrantNotIssuable) {
		t.Errorf("reissuing an expired grant: %v", err)
	}
	if a := f.actions("commerce.authorization.grant_issued"); len(a) != 1 {
		t.Errorf("audit = %v, want one reissue", a)
	}
}

// The customer approves; the merchant, not the customer, gets the grant.
func TestPerTransaction_TheMerchantIssuesTheGrantAfterApproval(t *testing.T) {
	f := newGrantsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.PerTransaction)
	w := f.mustSubmit(t, f.laptop(m, "c1"))
	if w.Grant != "" || w.Authorization != nil {
		t.Fatalf("awaiting checkout has a grant: %+v", w)
	}
	if _, err := f.issue.Run(ctx, shopA1, w.ID); !errors.Is(err, commerceapp.ErrGrantNotIssuable) {
		t.Errorf("issuing before approval: %v", err)
	}
	approved, err := f.decideCk.Run(ctx, f.token(), w.Snapshot.Hash(), true)
	if err != nil || approved.Status != commerce.CheckoutAuthorized || approved.Grant != "" || approved.Authorization == nil || approved.Authorization.GrantHash != "" {
		t.Fatalf("the customer's approval: %+v %v", approved, err)
	}
	g, err := f.issue.Run(ctx, shopA1, w.ID)
	if err != nil || g.Grant == "" {
		t.Fatalf("issue: %+v %v", g, err)
	}
	if _, _, err := f.redeemC(g, g.Grant); err != nil {
		t.Errorf("redeem: %v", err)
	}
}

func TestCancelAndRevocationRevokeUnredeemedGrants(t *testing.T) {
	f := newGrantsFixture(t)
	ctx := context.Background()
	m := f.mandate(t, "m1", commerce.Autonomous)
	cancelled := f.authorized(t, m, "c1", 1000)
	redeemed := f.authorized(t, m, "c2", 1000)
	open := f.authorized(t, m, "c3", 1000)
	if _, _, err := f.redeemC(redeemed, redeemed.Grant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cancel.Run(ctx, shopA1, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.redeemC(cancelled, cancelled.Grant); !errors.Is(err, commerce.ErrGrantRevoked) {
		t.Errorf("a cancelled checkout's grant: %v", err)
	}
	if _, err := f.cancel.Run(ctx, shopA1, redeemed.ID); !errors.Is(err, commerce.ErrCheckoutNotCancelable) {
		t.Errorf("cancelling a redeemed checkout: %v", err)
	}

	f.revokeConfirmed(t, m.ID)
	if _, _, err := f.redeemC(open, open.Grant); !errors.Is(err, commerce.ErrGrantRevoked) {
		t.Errorf("after the mandate's revocation: %v", err)
	}
	if got, _ := f.getCk.Run(ctx, shopA1, open.ID); got.Status != commerce.CheckoutRevoked {
		t.Errorf("open checkout after revocation: %s", got.Status)
	}
	if got, _ := f.getCk.Run(ctx, shopA1, redeemed.ID); got.Status != commerce.CheckoutRedeemed {
		t.Errorf("redeemed checkout after revocation: %s", got.Status)
	}
	if _, created, err := f.redeemC(redeemed, redeemed.Grant); err != nil || created {
		t.Errorf("repeating a redemption made before the revocation: %v %v", created, err)
	}
}

func TestRedeemGrant_ConcurrentRedemptionsMakeOneExecution(t *testing.T) {
	f := newGrantsFixture(t)
	m := f.mandate(t, "m1", commerce.Autonomous)
	c := f.authorized(t, m, "c1", 1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids, created := map[commerce.ExecutionID]bool{}, 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, made, err := f.redeemC(c, c.Grant)
			if err != nil {
				t.Errorf("redeem: %v", err)
				return
			}
			mu.Lock()
			ids[e.ID] = true
			if made {
				created++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != 1 || created != 1 {
		t.Errorf("%d executions, %d created; want 1 and 1", len(ids), created)
	}
}
