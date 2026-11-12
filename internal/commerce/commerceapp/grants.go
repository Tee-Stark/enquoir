package commerceapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
)

var (
	// ErrGrantInvalid: no grant of this merchant's is that secret - made
	// up, another merchant's, or replaced by a reissue.
	ErrGrantInvalid = errors.New("the grant is not valid for this merchant")
	// ErrGrantNotIssuable: the checkout has no active authorization -
	// not authorized yet, expired, cancelled or revoked.
	ErrGrantNotIssuable = errors.New("the checkout has no active authorization to issue a grant for")
	// ErrMandateNotActive: the mandate is paused, revoked or expired at
	// redemption, so the grant can't be redeemed now.
	ErrMandateNotActive = errors.New("the mandate is not active, so the grant can't be redeemed now")
)

const auditTargetAuthorization = "commerce_authorization"

// IssueGrant issues a grant for an authorized checkout - the first, for
// one the customer approved, or a replacement for a lost one. The
// earlier secret stops working; the expiry stays the authorization's.
type IssueGrant struct {
	Checkouts CheckoutStore
	Secrets   app.SecretGenerator
	Clock     app.Clock
	Audit     *app.Audit
}

func (g *IssueGrant) Run(ctx context.Context, p MerchantPrincipal, id commerce.CheckoutID) (CheckoutView, error) {
	now := g.Clock.Now()
	var a commerce.Authorization
	secret := mintGrant(g.Secrets, &a, now)
	c, err := g.Checkouts.IssueGrant(ctx, p.Scope(), id, a.GrantHash, now)
	if err != nil {
		return CheckoutView{}, err
	}
	g.Audit.Record(ctx, p.OrgID, app.AuditGrantIssued, auditTargetAuthorization, string(c.Authorization.ID), map[string]string{
		"merchant_id": string(p.MerchantID), "checkout_id": string(c.ID), "expires_at": c.Authorization.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
	v := checkoutView(c, now)
	v.Grant = secret
	return v, nil
}

// RedeemGrant is the merchant claiming a grant before charging, naming
// the checkout it is for. It creates the one execution the grant
// permits; repeating it returns that execution and never makes another.
type RedeemGrant struct {
	Checkouts CheckoutStore
	IDs       app.IDGenerator
	Clock     app.Clock
	Audit     *app.Audit
}

func (r *RedeemGrant) Run(ctx context.Context, p MerchantPrincipal, grant, externalCheckoutID, checkoutHash string) (commerce.Execution, bool, error) {
	if !strings.HasPrefix(grant, grantPrefix+"_") {
		return commerce.Execution{}, false, ErrGrantInvalid
	}
	ext := commerce.ExternalID(strings.TrimSpace(externalCheckoutID))
	now := r.Clock.Now()
	e, created, err := r.Checkouts.RedeemGrant(ctx, p.Scope(), app.HashSecret(grant), now,
		func(a commerce.Authorization, m MandateRow, f commerce.FundingMethod) (commerce.Execution, error) {
			if err := a.CheckRedemption(now, ext, checkoutHash); err != nil {
				return commerce.Execution{}, err
			}
			if m.StatusAt(now, m.Deps) != commerce.MandateActive {
				return commerce.Execution{}, ErrMandateNotActive
			}
			return commerce.NewExecution(commerce.ExecutionID(r.IDs.NewID("exe")), a, f, now), nil
		})
	if err != nil {
		return commerce.Execution{}, false, err
	}
	if !created {
		if e.ExternalCheckoutID != ext || e.CheckoutHash != checkoutHash {
			return commerce.Execution{}, false, commerce.ErrRedemptionMismatch
		}
		return e, false, nil
	}
	r.Audit.Record(ctx, p.OrgID, app.AuditGrantRedeemed, auditTargetAuthorization, string(e.AuthorizationID), map[string]string{
		"merchant_id": string(p.MerchantID), "checkout_id": string(e.CheckoutID), "execution_id": string(e.ID),
		"mandate_id": string(e.MandateID), "amount_minor": fmt.Sprint(e.AmountMinor), "currency": string(e.Currency),
	})
	return e, true, nil
}

// GetExecution fetches one of the merchant's executions.
type GetExecution struct{ Checkouts CheckoutStore }

func (g *GetExecution) Run(ctx context.Context, p MerchantPrincipal, id commerce.ExecutionID) (commerce.Execution, error) {
	return g.Checkouts.GetExecution(ctx, p.Scope(), id)
}
