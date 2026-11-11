package commerce

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"enquoir/internal/domain"
)

// FundingMethodID identifies a funding-method binding: Enquoir's own,
// opaque reference to one saved payment method of one customer. The
// merchant maps it to its PSP token; Enquoir never sees that token.
type FundingMethodID string

// FundingReference is the merchant's own ID for a saved payment method
// (its "mfm_..."), unique per merchant. It is an ExternalID that must
// not look like a card number.
type FundingReference ExternalID

// minPANDigits is the shortest card number (PAN) in use. A reference
// holding this many digits in a row - spaces and dashes ignored - may
// be a card number, and is refused rather than stored.
const minPANDigits = 12

// ErrLooksLikeCardNumber means a value holds a run of digits as long as
// a card number. Enquoir never accepts card data, so it is refused.
var ErrLooksLikeCardNumber = errors.New("must not contain a card number: send your own reference for the payment method, never the card")

// holdsCardNumber reports whether s has minPANDigits or more digits in a
// row, ignoring the spaces and dashes card numbers are written with.
func holdsCardNumber(s string) bool {
	run := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if run++; run >= minPANDigits {
				return true
			}
		case r == ' ' || r == '-':
		default:
			run = 0
		}
	}
	return false
}

// NewFundingReference checks a merchant's payment-method reference.
func NewFundingReference(s string) (FundingReference, error) {
	if holdsCardNumber(s) {
		return "", fmt.Errorf("reference %w", ErrLooksLikeCardNumber)
	}
	ext, err := NewExternalID(s)
	if err != nil {
		return "", err
	}
	return FundingReference(ext), nil
}

// Provider names the merchant's payment service provider: "stripe",
// "adyen", a mock PSP. Lower-cased; informational - Enquoir never calls
// the provider.
type Provider string

var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,39}$`)

func NewProvider(s string) (Provider, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", errors.New("provider is required")
	}
	if !providerPattern.MatchString(s) {
		return "", errors.New("provider must be 1 to 40 lower-case letters, digits, '_', '.' or '-', starting with a letter or digit")
	}
	if holdsCardNumber(s) {
		return "", fmt.Errorf("provider %w", ErrLooksLikeCardNumber)
	}
	return Provider(s), nil
}

// FieldError is a validation error that names the field at fault.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

// FundingDisplay is what a consent screen shows the customer to
// recognise the payment method: a brand ("visa", "paypal") and, for
// cards and accounts, the last four digits. Nothing more of the card.
type FundingDisplay struct {
	Brand    string
	LastFour string
}

var (
	brandPattern    = regexp.MustCompile(`^[a-z][a-z_-]{0,29}$`)
	lastFourPattern = regexp.MustCompile(`^[0-9]{4}$`)
)

// NewFundingDisplay checks display details: a brand of 1 to 30
// lower-case letters, '_' or '-' (no digits, so nothing of a card fits
// in it), and last four exactly four digits, or empty.
func NewFundingDisplay(brand, lastFour string) (FundingDisplay, error) {
	brand = strings.ToLower(strings.TrimSpace(brand))
	if brand == "" {
		return FundingDisplay{}, &FieldError{Field: "brand", Err: errors.New("brand is required")}
	}
	if !brandPattern.MatchString(brand) {
		return FundingDisplay{}, &FieldError{Field: "brand", Err: errors.New("brand must be 1 to 30 lower-case letters, '_' or '-'")}
	}
	lastFour = strings.TrimSpace(lastFour)
	if lastFour != "" && !lastFourPattern.MatchString(lastFour) {
		return FundingDisplay{}, &FieldError{Field: "last_four", Err: errors.New("last_four must be exactly 4 digits, or omitted")}
	}
	return FundingDisplay{Brand: brand, LastFour: lastFour}, nil
}

// FundingMethod is a funding-method binding. Registering one grants no
// spending authority: only the customer's mandate does that.
type FundingMethod struct {
	ID         FundingMethodID
	MerchantID MerchantID
	OrgID      domain.OrgID
	CustomerID CustomerID
	Reference  FundingReference
	Provider   Provider
	Display    FundingDisplay
	Status     Status
	CreatedAt  time.Time
	DisabledAt *time.Time
}

// SameBinding reports whether two bindings of one reference describe the
// same payment method: same customer, provider and display details.
func (f FundingMethod) SameBinding(o FundingMethod) bool {
	return f.CustomerID == o.CustomerID && f.Provider == o.Provider && f.Display == o.Display
}
