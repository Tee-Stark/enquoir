package commerce

import (
	"errors"
	"time"

	"enquoir/internal/domain"
)

// AuthorizationID identifies Enquoir's permission for one authorized
// checkout to be charged once.
type AuthorizationID string

// ExecutionID identifies a redemption's execution: the one payment the
// merchant may now make.
type ExecutionID string

// AuthorizationTTL is how long an authorization - and any grant issued
// for it - can be redeemed, from the moment the checkout was authorized.
// Reissuing a grant does not extend it.
const AuthorizationTTL = 15 * time.Minute

// AuthorizationStatus is where an authorization stands.
type AuthorizationStatus string

const (
	AuthorizationActive   AuthorizationStatus = "active"
	AuthorizationRedeemed AuthorizationStatus = "redeemed"
	AuthorizationExpired  AuthorizationStatus = "expired"
	AuthorizationRevoked  AuthorizationStatus = "revoked"
)

var (
	// ErrGrantExpired: the grant was not redeemed within its window.
	ErrGrantExpired = errors.New("the grant has expired")
	// ErrGrantRevoked: the checkout was cancelled, or its mandate revoked.
	ErrGrantRevoked = errors.New("the grant was revoked: the checkout was cancelled or its mandate revoked")
	// ErrGrantRedeemed: the grant was already redeemed (for reissuing).
	ErrGrantRedeemed = errors.New("the grant was already redeemed")
	// ErrRedemptionMismatch: the redemption names another checkout, or
	// another version of it, than the one authorized.
	ErrRedemptionMismatch = errors.New("the grant is for another checkout, or another version of it")
)

// Authorization is Enquoir's permission for one authorized checkout to
// be charged once: the exact amount, currency, funding method and
// checkout version. Its grant - the secret the merchant redeems - is
// stored only as a hash, and may be reissued until it is redeemed.
type Authorization struct {
	ID                 AuthorizationID
	CheckoutID         CheckoutID
	ExternalCheckoutID ExternalID
	MandateID          MandateID
	OrgID              domain.OrgID
	MerchantID         MerchantID
	CheckoutHash       string
	AmountMinor        int64
	Currency           domain.Currency
	FundingMethodID    FundingMethodID
	GrantHash          string // empty until a grant is issued
	GrantIssuedAt      *time.Time
	CreatedAt          time.Time
	ExpiresAt          time.Time
	RedeemedAt         *time.Time
	RevokedAt          *time.Time
	ExecutionID        ExecutionID
}

// NewAuthorization authorizes checkout c at now, for AuthorizationTTL.
func NewAuthorization(id AuthorizationID, c Checkout, now time.Time) Authorization {
	s := c.Snapshot
	return Authorization{
		ID: id, CheckoutID: c.ID, ExternalCheckoutID: s.ExternalID, MandateID: s.MandateID, OrgID: s.OrgID, MerchantID: s.MerchantID,
		CheckoutHash: s.Hash(), AmountMinor: s.TotalMinor, Currency: s.Currency, FundingMethodID: s.FundingMethodID,
		CreatedAt: now, ExpiresAt: now.Add(AuthorizationTTL),
	}
}

// StatusAt is redeemed, else revoked, else expired, else active.
func (a Authorization) StatusAt(now time.Time) AuthorizationStatus {
	switch {
	case a.RedeemedAt != nil:
		return AuthorizationRedeemed
	case a.RevokedAt != nil:
		return AuthorizationRevoked
	case !now.Before(a.ExpiresAt):
		return AuthorizationExpired
	}
	return AuthorizationActive
}

// CheckRedemption is whether the grant may be redeemed at now for the
// checkout the merchant names: not redeemed, not revoked, not expired,
// and exactly this checkout - its external ID and hash.
func (a Authorization) CheckRedemption(now time.Time, externalID ExternalID, checkoutHash string) error {
	switch a.StatusAt(now) {
	case AuthorizationRedeemed:
		return ErrGrantRedeemed
	case AuthorizationRevoked:
		return ErrGrantRevoked
	case AuthorizationExpired:
		return ErrGrantExpired
	}
	if externalID != a.ExternalCheckoutID || checkoutHash != a.CheckoutHash {
		return ErrRedemptionMismatch
	}
	return nil
}

// ExecutionStatus is where the payment a redemption permits stands.
// Phase 6 reports its outcome; until then it is pending.
type ExecutionStatus string

const ExecutionPending ExecutionStatus = "pending"

// Execution is the one payment a redeemed grant permits: what to charge
// and with which saved payment method. A grant has at most one.
type Execution struct {
	ID                 ExecutionID
	AuthorizationID    AuthorizationID
	CheckoutID         CheckoutID
	ExternalCheckoutID ExternalID
	CheckoutHash       string
	MandateID          MandateID
	OrgID              domain.OrgID
	MerchantID         MerchantID
	AmountMinor        int64
	Currency           domain.Currency
	FundingMethod      FundingMethod
	Status             ExecutionStatus
	CreatedAt          time.Time
}

// NewExecution is the execution redeeming a at now permits, charging f.
func NewExecution(id ExecutionID, a Authorization, f FundingMethod, now time.Time) Execution {
	return Execution{
		ID: id, AuthorizationID: a.ID, CheckoutID: a.CheckoutID, ExternalCheckoutID: a.ExternalCheckoutID, CheckoutHash: a.CheckoutHash,
		MandateID: a.MandateID, OrgID: a.OrgID, MerchantID: a.MerchantID, AmountMinor: a.AmountMinor, Currency: a.Currency,
		FundingMethod: f, Status: ExecutionPending, CreatedAt: now,
	}
}
