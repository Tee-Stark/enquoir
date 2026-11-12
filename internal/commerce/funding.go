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
// opaque reference to one saved payment method of one customer.
type FundingMethodID string

// FundingReference is the merchant's own ID for a saved payment method
// (its "mfm_..."), unique per merchant. It is an ExternalID that must
// not look like a card number.
type FundingReference ExternalID

// minPANDigits is the shortest card number (PAN) in use. A reference
// holding this many digits in a row - common separators ignored - may
// be a card number, and is refused rather than stored.
const minPANDigits = 12

// ErrLooksLikeCardNumber means a value holds a run of digits as long as
// a card number. Enquoir never accepts card data, so it is refused.
var ErrLooksLikeCardNumber = errors.New("must not contain a card number: send your own reference for the payment method, never the card")

// isDigitSeparator reports whether r is a character people commonly write
// a card number's digit groups apart with. A guard that only recognized
// some of these would be defeated by simply picking a different one; see
// docs/progress.md.
func isDigitSeparator(r rune) bool {
	switch r {
	case ' ', '-', '.', '_':
		return true
	}
	return false
}

// asciiDigit reports whether r is a decimal digit - including the
// full-width Unicode forms (U+FF10-U+FF19), which a plain '0'-'9' range
// check would miss - and returns its value as an ASCII digit byte.
func asciiDigit(r rune) (byte, bool) {
	switch {
	case r >= '0' && r <= '9':
		return byte(r), true
	case r >= 0xFF10 && r <= 0xFF19:
		return byte(r-0xFF10) + '0', true
	}
	return 0, false
}

// HoldsCardNumber reports whether s has minPANDigits or more digits in a
// row, ignoring any separator a digit group is commonly written apart with.
// Exported so every merchant-supplied free-text field across the commerce
// package - not just an allowlist of call sites here - can check itself.
func HoldsCardNumber(s string) bool {
	run := 0
	for _, r := range s {
		if _, ok := asciiDigit(r); ok {
			if run++; run >= minPANDigits {
				return true
			}
			continue
		}
		if !isDigitSeparator(r) {
			run = 0
		}
	}
	return false
}

// NewFundingReference checks a merchant's payment-method reference: no
// run of 12 or more digits, which may be a card number.
func NewFundingReference(s string) (FundingReference, error) {
	if HoldsCardNumber(s) {
		return "", fmt.Errorf("reference %w", ErrLooksLikeCardNumber)
	}
	ext, err := NewExternalID(s)
	if err != nil {
		return "", err
	}
	return FundingReference(ext), nil
}

// ProviderReference is the PSP's own ID for a saved payment method:
// Stripe's "pm_...", Adyen's storedPaymentMethodId. It identifies the
// method at the PSP, and charges nothing without the merchant's PSP
// secret key, which Enquoir never holds. Unique per merchant and PSP.
type ProviderReference string

// NewProviderReference checks a PSP's reference. Adyen's are 16-digit
// numbers, so digits alone are allowed; a card number - minPANDigits (the
// shortest PAN) to 19 digits, common separators ignored - with a valid
// Luhn checksum - is refused.
func NewProviderReference(s string) (ProviderReference, error) {
	if holdsLuhnCardNumber(s) {
		return "", fmt.Errorf("provider reference %w", ErrLooksLikeCardNumber)
	}
	s, err := checkExternalIDShape(s)
	if err != nil {
		return "", err
	}
	return ProviderReference(s), nil
}

// holdsLuhnCardNumber reports whether a whole run of minPANDigits to 19
// digits in s (common separators inside it ignored) passes the Luhn
// check, as every card number does. About one random 16-digit token in
// ten passes too; see docs/progress.md.
func holdsLuhnCardNumber(s string) bool {
	var runs [][]byte
	var cur []byte
	for _, r := range s {
		if d, ok := asciiDigit(r); ok {
			cur = append(cur, d)
			continue
		}
		if !isDigitSeparator(r) {
			runs, cur = append(runs, cur), nil
		}
	}
	runs = append(runs, cur)
	for _, r := range runs {
		if len(r) >= minPANDigits && len(r) <= 19 && luhn(r) {
			return true
		}
	}
	return false
}

func luhn(digits []byte) bool {
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum, double = sum+d, !double
	}
	return sum%10 == 0
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
	if HoldsCardNumber(s) {
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
	// Reference is the merchant's own ID for the method, ProviderReference
	// the PSP's; a binding has at least one.
	Reference         FundingReference
	ProviderReference ProviderReference
	Provider          Provider
	Display           FundingDisplay
	Status            Status
	CreatedAt         time.Time
	DisabledAt        *time.Time
}

// Compatible reports whether o, a repeated registration that resolved to
// the stored binding f, describes the same payment method: same customer,
// provider and display details, and each reference o names is f's. o may
// leave out a reference f has (resolving by the other); it may not add or
// change one - that would be an update, not a repeat.
func (f FundingMethod) Compatible(o FundingMethod) bool {
	return f.CustomerID == o.CustomerID && f.Provider == o.Provider && f.Display == o.Display &&
		(o.Reference == "" || o.Reference == f.Reference) &&
		(o.ProviderReference == "" || o.ProviderReference == f.ProviderReference)
}
