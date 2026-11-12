package commercetest

import (
	"context"
	"sync"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

var _ commerceapp.CheckoutStore = (*Checkouts)(nil)

// Checkouts is an in-memory commerceapp.CheckoutStore. One lock stands
// in for the Postgres lock on each mandate: decisions, redemptions,
// cancellations and revocations are serialised.
type Checkouts struct {
	mu         sync.Mutex
	checkouts  map[commerce.CheckoutID]commerce.Checkout
	order      []commerce.CheckoutID
	executions map[commerce.ExecutionID]commerce.Execution
	mandates   *Mandates
	tokens     app.TokenStore
}

// NewCheckouts also registers with mandates, so confirming a mandate's
// revocation revokes its unredeemed grants.
func NewCheckouts(mandates *Mandates, tokens app.TokenStore) *Checkouts {
	s := &Checkouts{checkouts: map[commerce.CheckoutID]commerce.Checkout{}, executions: map[commerce.ExecutionID]commerce.Execution{},
		mandates: mandates, tokens: tokens}
	mandates.OnRevoked = s.revokeGrants
	return s
}

func scopeOfCheckout(c commerce.Checkout) commerceapp.Scope {
	return commerceapp.Scope{OrgID: c.Snapshot.OrgID, MerchantID: c.Snapshot.MerchantID}
}

// usage is what id counts at now. Callers hold mu.
func (s *Checkouts) usage(id commerce.MandateID, now time.Time) commerce.MandateUsage {
	var u commerce.MandateUsage
	for _, c := range s.checkouts {
		if c.Snapshot.MandateID == id && c.CountsAt(now) {
			u.Purchases++
			u.SpentMinor += c.Snapshot.TotalMinor
		}
	}
	return u
}

func (s *Checkouts) apply(c *commerce.Checkout, o commerceapp.CheckoutOutcome, at time.Time) {
	c.Status, c.Denials, c.ApprovalExpiresAt, c.Authorization = o.Status, o.Denials, o.ApprovalExpiresAt, o.Authorization
	if o.Status != commerce.CheckoutAwaitingApproval {
		c.DecidedAt = &at
	}
}

func (s *Checkouts) SubmitCheckout(ctx context.Context, sc commerceapp.Scope, c commerce.Checkout, decide commerceapp.CheckoutDecider) (commerce.Checkout, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		have := s.checkouts[id]
		if scopeOfCheckout(have) == sc && have.Snapshot.ExternalID == c.Snapshot.ExternalID {
			return have, false, nil
		}
	}
	row, err := s.mandates.GetMandate(ctx, sc, c.Snapshot.MandateID)
	if err != nil {
		return commerce.Checkout{}, false, err
	}
	s.apply(&c, decide(row, s.usage(row.ID, c.CreatedAt)), c.CreatedAt)
	s.checkouts[c.ID] = c
	s.order = append(s.order, c.ID)
	return c, true, nil
}

func (s *Checkouts) GetCheckout(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID) (commerce.Checkout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkouts[id]
	if !ok || scopeOfCheckout(c) != sc {
		return commerce.Checkout{}, app.ErrNotFound
	}
	return c, nil
}

func (s *Checkouts) CheckoutForApproval(ctx context.Context, id commerce.CheckoutID) (commerce.Checkout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkouts[id]
	if !ok {
		return commerce.Checkout{}, app.ErrNotFound
	}
	return c, nil
}

func (s *Checkouts) DecideCheckout(ctx context.Context, tokenHash string, id commerce.CheckoutID, at time.Time, decide commerceapp.CheckoutDecider) (commerce.Checkout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkouts[id]
	if !ok || c.StatusAt(at) != commerce.CheckoutAwaitingApproval {
		return commerce.Checkout{}, app.ErrTokenInvalid
	}
	tok, err := s.tokens.Consume(ctx, tokenHash, app.PurposeCheckoutApproval, at)
	if err != nil || tok.Subject != string(id) {
		return commerce.Checkout{}, app.ErrTokenInvalid
	}
	row, err := s.mandates.GetMandate(ctx, scopeOfCheckout(c), c.Snapshot.MandateID)
	if err != nil {
		return commerce.Checkout{}, err
	}
	s.apply(&c, decide(row, s.usage(row.ID, at)), at)
	s.checkouts[id] = c
	return c, nil
}

func (s *Checkouts) CancelCheckout(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID, at time.Time) (commerce.Checkout, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkouts[id]
	if !ok || scopeOfCheckout(c) != sc {
		return commerce.Checkout{}, false, app.ErrNotFound
	}
	switch c.StatusAt(at) {
	case commerce.CheckoutCanceled:
		return c, false, nil
	case commerce.CheckoutAuthorized, commerce.CheckoutAwaitingApproval:
		c.Status, c.CanceledAt = commerce.CheckoutCanceled, &at
		if c.Authorization != nil {
			a := *c.Authorization
			a.RevokedAt = &at
			c.Authorization = &a
		}
		s.checkouts[id] = c
		return c, true, nil
	}
	return commerce.Checkout{}, false, commerce.ErrCheckoutNotCancelable
}

func (s *Checkouts) IssueGrant(ctx context.Context, sc commerceapp.Scope, id commerce.CheckoutID, grantHash string, at time.Time) (commerce.Checkout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkouts[id]
	if !ok || scopeOfCheckout(c) != sc {
		return commerce.Checkout{}, app.ErrNotFound
	}
	if c.Authorization == nil {
		return commerce.Checkout{}, commerceapp.ErrGrantNotIssuable
	}
	a := *c.Authorization
	switch a.StatusAt(at) {
	case commerce.AuthorizationRedeemed:
		return commerce.Checkout{}, commerce.ErrGrantRedeemed
	case commerce.AuthorizationActive:
	default:
		return commerce.Checkout{}, commerceapp.ErrGrantNotIssuable
	}
	a.GrantHash, a.GrantIssuedAt = grantHash, &at
	c.Authorization = &a
	s.checkouts[id] = c
	return c, nil
}

func (s *Checkouts) RedeemGrant(ctx context.Context, sc commerceapp.Scope, grantHash string, at time.Time, redeem commerceapp.GrantRedeemer) (commerce.Execution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c commerce.Checkout
	found := false
	for _, have := range s.checkouts {
		if scopeOfCheckout(have) == sc && have.Authorization != nil && grantHash != "" && have.Authorization.GrantHash == grantHash {
			c, found = have, true
			break
		}
	}
	if !found {
		return commerce.Execution{}, false, commerceapp.ErrGrantInvalid
	}
	a := *c.Authorization
	if a.RedeemedAt != nil {
		return s.executions[a.ExecutionID], false, nil
	}
	row, err := s.mandates.GetMandate(ctx, sc, a.MandateID)
	if err != nil {
		return commerce.Execution{}, false, err
	}
	f, err := s.mandates.methods.GetFundingMethod(ctx, sc, a.FundingMethodID)
	if err != nil {
		return commerce.Execution{}, false, err
	}
	e, err := redeem(a, row, f)
	if err != nil {
		return commerce.Execution{}, false, err
	}
	a.RedeemedAt, a.ExecutionID = &at, e.ID
	c.Authorization, c.Status = &a, commerce.CheckoutRedeemed
	s.checkouts[c.ID] = c
	s.executions[e.ID] = e
	return e, true, nil
}

func (s *Checkouts) GetExecution(ctx context.Context, sc commerceapp.Scope, id commerce.ExecutionID) (commerce.Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.executions[id]
	if !ok || (commerceapp.Scope{OrgID: e.OrgID, MerchantID: e.MerchantID}) != sc {
		return commerce.Execution{}, app.ErrNotFound
	}
	return e, nil
}

// revokeGrants revokes a revoked mandate's unredeemed grants.
func (s *Checkouts) revokeGrants(id commerce.MandateID, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for cid, c := range s.checkouts {
		if c.Snapshot.MandateID != id || c.Status != commerce.CheckoutAuthorized || c.Authorization == nil || c.Authorization.RedeemedAt != nil {
			continue
		}
		a := *c.Authorization
		a.RevokedAt = &at
		c.Authorization, c.Status = &a, commerce.CheckoutRevoked
		s.checkouts[cid] = c
	}
}
