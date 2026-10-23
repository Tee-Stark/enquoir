package domain

import (
	"strings"
	"testing"
)

var testAddress = BeneficiaryAddress{
	Street:   "412 5th Avenue",
	City:     "Seattle",
	State:    "WA",
	Postcode: "98104",
}

func mustBeneficiary(t *testing.T, name, accountNumber, routingNumber string, addr BeneficiaryAddress) Beneficiary {
	t.Helper()
	b, err := NewBeneficiary(name, accountNumber, routingNumber, addr)
	if err != nil {
		t.Fatalf("NewBeneficiary(%q, %q, %q, %+v): unexpected error: %v", name, accountNumber, routingNumber, addr, err)
	}
	return b
}

func TestBeneficiaryKey_SameDetailsGiveSameKey(t *testing.T) {
	a := mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", testAddress)
	b := mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", testAddress)
	if a.Key != b.Key {
		t.Errorf("identical beneficiaries produced different keys: %q vs %q", a.Key, b.Key)
	}
}

func TestBeneficiaryKey_ChangingAnyFieldChangesKey(t *testing.T) {
	base := mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", testAddress)

	otherAddr := func(mutate func(*BeneficiaryAddress)) BeneficiaryAddress {
		a := testAddress
		mutate(&a)
		return a
	}
	variants := []Beneficiary{
		mustBeneficiary(t, "Other Ltd", "50001121", "021000021", testAddress),
		mustBeneficiary(t, "Acme Ltd", "50001122", "021000021", testAddress),
		mustBeneficiary(t, "Acme Ltd", "50001121", "011000015", testAddress),
		mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", otherAddr(func(a *BeneficiaryAddress) { a.Street = "1 Main Street" })),
		mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", otherAddr(func(a *BeneficiaryAddress) { a.City = "Tacoma" })),
		mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", otherAddr(func(a *BeneficiaryAddress) { a.State = "OR" })),
		mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", otherAddr(func(a *BeneficiaryAddress) { a.Postcode = "98105" })),
	}

	for i, v := range variants {
		if v.Key == base.Key {
			t.Errorf("variant %d: expected a different key from base, got the same %q", i, v.Key)
		}
	}
}

func TestBeneficiaryKey_NormalisesWhitespace(t *testing.T) {
	canonical := mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", testAddress)
	padded := mustBeneficiary(t, "  Acme Ltd ", " 50001121 ", " 021000021", BeneficiaryAddress{
		Street:   " 412 5th Avenue ",
		City:     "Seattle ",
		State:    " WA",
		Postcode: "98104 ",
	})

	if canonical.Key != padded.Key {
		t.Errorf("normalised inputs should key the same: %q vs %q", canonical.Key, padded.Key)
	}
	if padded.Name != "Acme Ltd" || padded.Address.Street != "412 5th Avenue" {
		t.Errorf("fields not trimmed: %+v", padded)
	}
}

func TestBeneficiaryKey_IsSafeLengthAndCharset(t *testing.T) {
	key := string(mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", testAddress).Key)
	if len(key) != 32 {
		t.Errorf("key length %d, want 32: %q", len(key), key)
	}
	if !safeKeyCharset.MatchString(key) {
		t.Errorf("key contains unsafe characters: %q", key)
	}
}

func TestNewBeneficiary_RejectsInvalidDetails(t *testing.T) {
	withAddr := func(mutate func(*BeneficiaryAddress)) BeneficiaryAddress {
		a := testAddress
		mutate(&a)
		return a
	}

	tests := []struct {
		name          string
		beneficiary   string
		accountNumber string
		routingNumber string
		addr          BeneficiaryAddress
		wantErr       string
	}{
		{"missing name", "  ", "50001121", "021000021", testAddress, "name is required"},
		{"account number with letters", "Acme Ltd", "5000A121", "021000021", testAddress, "account number"},
		{"account number too short", "Acme Ltd", "123", "021000021", testAddress, "account number"},
		{"account number too long", "Acme Ltd", "123456789012345678", "021000021", testAddress, "account number"},
		{"routing number not nine digits", "Acme Ltd", "50001121", "02100002", testAddress, "routing number"},
		{"routing number with letters", "Acme Ltd", "50001121", "02100002X", testAddress, "routing number"},
		{"routing number fails checksum", "Acme Ltd", "50001121", "021000022", testAddress, "routing number"},
		{"missing street", "Acme Ltd", "50001121", "021000021", withAddr(func(a *BeneficiaryAddress) { a.Street = "" }), "street"},
		{"missing city", "Acme Ltd", "50001121", "021000021", withAddr(func(a *BeneficiaryAddress) { a.City = " " }), "city"},
		{"missing state", "Acme Ltd", "50001121", "021000021", withAddr(func(a *BeneficiaryAddress) { a.State = "" }), "state"},
		{"missing postcode", "Acme Ltd", "50001121", "021000021", withAddr(func(a *BeneficiaryAddress) { a.Postcode = "" }), "postcode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewBeneficiary(tt.beneficiary, tt.accountNumber, tt.routingNumber, tt.addr)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}
