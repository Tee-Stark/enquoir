package commerce

import (
	"errors"
	"testing"
	"time"
)

func authorizedLaptop(t *testing.T) (Checkout, Authorization) {
	t.Helper()
	snap, err := NewCheckoutSnapshot(laptop())
	if err != nil {
		t.Fatal(err)
	}
	c := Checkout{ID: "chk_1", Snapshot: snap, Status: CheckoutAuthorized, CreatedAt: now}
	return c, NewAuthorization("auth_1", c, now)
}

func TestNewAuthorization_BindsTheCheckoutForFifteenMinutes(t *testing.T) {
	c, a := authorizedLaptop(t)
	if a.CheckoutID != c.ID || a.MandateID != c.Snapshot.MandateID || a.CheckoutHash != c.Snapshot.Hash() ||
		a.AmountMinor != c.Snapshot.TotalMinor || a.Currency != c.Snapshot.Currency || a.FundingMethodID != c.Snapshot.FundingMethodID ||
		a.MerchantID != c.Snapshot.MerchantID || !a.ExpiresAt.Equal(now.Add(15*time.Minute)) || a.GrantHash != "" {
		t.Errorf("authorization = %+v", a)
	}
	if a.StatusAt(now) != AuthorizationActive || a.StatusAt(a.ExpiresAt) != AuthorizationExpired {
		t.Error("active until its expiry, expired at it")
	}
}

func TestAuthorization_Status(t *testing.T) {
	_, a := authorizedLaptop(t)
	later := now.Add(time.Hour)
	r := a
	r.RedeemedAt, r.ExecutionID = &now, "exe_1"
	if r.StatusAt(later) != AuthorizationRedeemed {
		t.Error("a redeemed grant stays redeemed after its expiry")
	}
	v := a
	v.RevokedAt = &now
	if v.StatusAt(now) != AuthorizationRevoked {
		t.Error("revoked")
	}
}

// Redemption checks, in order: revoked, expired, the same checkout.
func TestAuthorization_CheckRedemption(t *testing.T) {
	c, a := authorizedLaptop(t)
	ext, hash := c.Snapshot.ExternalID, c.Snapshot.Hash()
	if err := a.CheckRedemption(now, ext, hash); err != nil {
		t.Fatalf("a valid redemption: %v", err)
	}
	cases := map[string]struct {
		a         func(Authorization) Authorization
		at        time.Time
		ext, hash string
		want      error
	}{
		"expired":          {func(a Authorization) Authorization { return a }, a.ExpiresAt, string(ext), hash, ErrGrantExpired},
		"revoked":          {func(a Authorization) Authorization { a.RevokedAt = &now; return a }, now, string(ext), hash, ErrGrantRevoked},
		"revoked, expired": {func(a Authorization) Authorization { a.RevokedAt = &now; return a }, a.ExpiresAt, string(ext), hash, ErrGrantRevoked},
		"another checkout": {func(a Authorization) Authorization { return a }, now, "checkout_988", hash, ErrRedemptionMismatch},
		"another hash":     {func(a Authorization) Authorization { return a }, now, string(ext), "sha256:00", ErrRedemptionMismatch},
		"no hash":          {func(a Authorization) Authorization { return a }, now, string(ext), "", ErrRedemptionMismatch},
		"already redeemed": {func(a Authorization) Authorization { a.RedeemedAt = &now; return a }, now, string(ext), hash, ErrGrantRedeemed},
	}
	for name, tc := range cases {
		x := tc.a(a)
		x.ExternalCheckoutID = ext
		if err := x.CheckRedemption(tc.at, ExternalID(tc.ext), tc.hash); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestCheckout_StatusFollowsItsAuthorization(t *testing.T) {
	c, a := authorizedLaptop(t)
	c.Authorization = &a
	if c.StatusAt(now) != CheckoutAuthorized {
		t.Error("authorized while its grant is valid")
	}
	if c.StatusAt(a.ExpiresAt) != CheckoutExpired {
		t.Error("an unredeemed grant past its expiry makes the checkout expired")
	}
	c.Status = CheckoutRedeemed
	if c.StatusAt(a.ExpiresAt.Add(time.Hour)) != CheckoutRedeemed {
		t.Error("redeemed stays redeemed")
	}
	// Counting against the mandate.
	c.Status = CheckoutAuthorized
	if !c.CountsAt(now) || c.CountsAt(a.ExpiresAt) {
		t.Error("an authorized checkout counts until its grant expires")
	}
	c.Status = CheckoutRedeemed
	if !c.CountsAt(a.ExpiresAt.Add(time.Hour)) {
		t.Error("a redeemed checkout keeps counting")
	}
	for _, st := range []CheckoutStatus{CheckoutCanceled, CheckoutRevoked, CheckoutDenied, CheckoutAwaitingApproval} {
		c.Status = st
		if c.CountsAt(now) {
			t.Errorf("%s counts", st)
		}
	}
}
