package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// Beneficiary is a payee's bank account, to be registered on the rail
// before any Intent can pay it. Only US company accounts paid in USD
// over the local (ACH) rail are supported for now.
type Beneficiary struct {
	Name          string
	AccountNumber string
	RoutingNumber string
	Address       BeneficiaryAddress
	Key           BeneficiaryKey
}

// NewBeneficiary builds a Beneficiary, trimming every field and
// validating the bank details before deriving its key.
func NewBeneficiary(name, accountNumber, routingNumber string, addr BeneficiaryAddress) (Beneficiary, error) {
	name = strings.TrimSpace(name)
	accountNumber = strings.TrimSpace(accountNumber)
	routingNumber = strings.TrimSpace(routingNumber)
	addr = BeneficiaryAddress{
		Street:   strings.TrimSpace(addr.Street),
		City:     strings.TrimSpace(addr.City),
		State:    strings.TrimSpace(addr.State),
		Postcode: strings.TrimSpace(addr.Postcode),
	}

	if name == "" {
		return Beneficiary{}, errors.New("name is required")
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

	b := Beneficiary{
		Name:          name,
		AccountNumber: accountNumber,
		RoutingNumber: routingNumber,
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
