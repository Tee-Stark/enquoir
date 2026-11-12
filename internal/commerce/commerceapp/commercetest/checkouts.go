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
// in for the Postgres lock on each mandate: decisions are serialised.
type Checkouts struct {
	mu        sync.Mutex
	checkouts map[commerce.CheckoutID]commerce.Checkout
	order     []commerce.CheckoutID
	mandates  *Mandates
	tokens    app.TokenStore
}

func NewCheckouts(mandates *Mandates, tokens app.TokenStore) *Checkouts {
	return &Checkouts{checkouts: map[commerce.CheckoutID]commerce.Checkout{}, mandates: mandates, tokens: tokens}
}

func scopeOfCheckout(c commerce.Checkout) commerceapp.Scope {
	return commerceapp.Scope{OrgID: c.Snapshot.OrgID, MerchantID: c.Snapshot.MerchantID}
}

// usage is what id has authorized. Callers hold mu.
func (s *Checkouts) usage(id commerce.MandateID) commerce.MandateUsage {
	var u commerce.MandateUsage
	for _, c := range s.checkouts {
		if c.Snapshot.MandateID == id && c.Status == commerce.CheckoutAuthorized {
			u.Purchases++
			u.SpentMinor += c.Snapshot.TotalMinor
		}
	}
	return u
}

func (s *Checkouts) apply(c *commerce.Checkout, o commerceapp.CheckoutOutcome, at time.Time) {
	c.Status, c.Denials, c.ApprovalExpiresAt = o.Status, o.Denials, o.ApprovalExpiresAt
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
	s.apply(&c, decide(row, s.usage(row.ID)), c.CreatedAt)
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
	s.apply(&c, decide(row, s.usage(row.ID)), at)
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
		s.checkouts[id] = c
		return c, true, nil
	}
	return commerce.Checkout{}, false, commerce.ErrCheckoutNotCancelable
}
