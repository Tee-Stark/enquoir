package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// BeneficiaryKey is a stable, deterministic key derived from a
// Beneficiary's details. The rail has no idempotency key for creating
// a beneficiary, so this key is what lets Enquoir recognise a re-sent
// registration instead of creating the same payee twice - which
// matters for money, not just tidiness: an Intent's key includes its
// payee's ID, so one real payee under two IDs would let the same
// invoice be paid twice.
type BeneficiaryKey string

// BeneficiaryAddress is a beneficiary's postal address.
type BeneficiaryAddress struct {
	Street   string
	City     string
	State    string
	Postcode string
}

// AccountCategory is a US bank account's type. ACH needs the right
// one - a payment sent as the wrong type can be returned - so it is
// always stated, never defaulted.
type AccountCategory string

const (
	Checking AccountCategory = "Checking"
	Savings  AccountCategory = "Savings"
)

// Beneficiary is a payee's bank account, to be registered on the rail
// before any Intent can pay it. Only US company accounts paid in USD
// over the local (ACH) rail are supported for now. Validation follows
// Airwallex's live US/USD/LOCAL/ACH beneficiary form schema, so a
// payee the rail would refuse is refused here first.
type Beneficiary struct {
	Name          string
	AccountNumber string
	RoutingNumber string
	Category      AccountCategory
	Address       BeneficiaryAddress // State is a two-letter code, e.g. "WA"
	Key           BeneficiaryKey
}

var (
	usStateCode = regexp.MustCompile(`^[A-Z]{2}$`)
	usPostcode  = regexp.MustCompile(`^[0-9]{5}(-[0-9]{4})?$`)
)

// NewBeneficiary builds a Beneficiary, trimming every field,
// normalising the account category and the state (both "WA" and
// "US-WA" become "WA"), and validating the details before deriving
// its key.
func NewBeneficiary(name, accountNumber, routingNumber string, category AccountCategory, addr BeneficiaryAddress) (Beneficiary, error) {
	name = strings.TrimSpace(name)
	accountNumber = strings.TrimSpace(accountNumber)
	routingNumber = strings.TrimSpace(routingNumber)
	addr = BeneficiaryAddress{
		Street:   strings.TrimSpace(addr.Street),
		City:     strings.TrimSpace(addr.City),
		State:    strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(addr.State)), "US-"),
		Postcode: strings.TrimSpace(addr.Postcode),
	}

	if name == "" {
		return Beneficiary{}, errors.New("name is required")
	}
	if len(name) < 2 || isDigits(name) {
		return Beneficiary{}, fmt.Errorf("invalid name %q: must be 2 to 200 characters and not only digits", name)
	}
	switch AccountCategory(strings.ToLower(strings.TrimSpace(string(category)))) {
	case "checking":
		category = Checking
	case "savings":
		category = Savings
	default:
		return Beneficiary{}, fmt.Errorf("invalid account category %q: must be Checking or Savings", category)
	}
	if !isDigits(accountNumber) || len(accountNumber) < 4 || len(accountNumber) > 17 {
		return Beneficiary{}, fmt.Errorf("invalid account number %q: must be 4 to 17 digits", accountNumber)
	}
	if !isValidABA(routingNumber) {
		return Beneficiary{}, fmt.Errorf("invalid routing number %q: must be a 9-digit ABA number with a valid checksum", routingNumber)
	}
	for _, f := range []struct{ name, value string }{
		{"street", addr.Street},
		{"city", addr.City},
		{"state", addr.State},
		{"postcode", addr.Postcode},
	} {
		if f.value == "" {
			return Beneficiary{}, fmt.Errorf("address %s is required", f.name)
		}
	}
	if len(addr.Street) < 5 || isDigits(addr.Street) {
		return Beneficiary{}, fmt.Errorf("invalid address street %q: must be 5 to 200 characters and not only digits", addr.Street)
	}
	if !usStateCode.MatchString(addr.State) {
		return Beneficiary{}, fmt.Errorf("invalid address state %q: must be a two-letter US state code such as WA", addr.State)
	}
	if !usPostcode.MatchString(addr.Postcode) {
		return Beneficiary{}, fmt.Errorf("invalid address postcode %q: must be a 5-digit ZIP or ZIP+4", addr.Postcode)
	}

	b := Beneficiary{
		Name:          name,
		AccountNumber: accountNumber,
		RoutingNumber: routingNumber,
		Category:      category,
		Address:       addr,
	}
	b.Key = deriveBeneficiaryKey(b)
	return b, nil
}

// deriveBeneficiaryKey hashes every normalised field, separated the
// same way deriveIntentKey's are, so no boundary shift can collide.
func deriveBeneficiaryKey(b Beneficiary) BeneficiaryKey {
	const sep = "\x00"
	canonical := strings.Join([]string{
		b.Name,
		b.AccountNumber,
		b.RoutingNumber,
		string(b.Category),
		b.Address.Street,
		b.Address.City,
		b.Address.State,
		b.Address.Postcode,
	}, sep)

	sum := sha256.Sum256([]byte(canonical))
	return BeneficiaryKey(hex.EncodeToString(sum[:])[:32])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isValidABA checks an ABA routing number's length and its standard
// 3-7-1 weighted checksum, catching a mistyped digit before it ever
// reaches the rail.
func isValidABA(s string) bool {
	if len(s) != 9 || !isDigits(s) {
		return false
	}
	weights := [9]int{3, 7, 1, 3, 7, 1, 3, 7, 1}
	sum := 0
	for i, w := range weights {
		sum += int(s[i]-'0') * w
	}
	return sum%10 == 0
}
