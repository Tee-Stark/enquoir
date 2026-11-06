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
	b, err := NewBeneficiary(name, accountNumber, routingNumber, Checking, addr)
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
			_, err := NewBeneficiary(tt.beneficiary, tt.accountNumber, tt.routingNumber, Checking, tt.addr)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewBeneficiary_AccountCategory(t *testing.T) {
	// Airwallex's US ACH schema requires Checking or Savings: the
	// wrong one can get an ACH payment returned, so it is never guessed.
	tests := []struct {
		in      AccountCategory
		want    AccountCategory
		wantErr bool
	}{
		{"Checking", Checking, false},
		{"savings", Savings, false},
		{" CHECKING ", Checking, false},
		{"", "", true},
		{"Current", "", true},
	}
	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			b, err := NewBeneficiary("Acme Ltd", "50001121", "021000021", tt.in, testAddress)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "account category") {
					t.Fatalf("want an account category error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b.Category != tt.want {
				t.Errorf("Category = %q, want %q", b.Category, tt.want)
			}
		})
	}

	checking, _ := NewBeneficiary("Acme Ltd", "50001121", "021000021", Checking, testAddress)
	savings, _ := NewBeneficiary("Acme Ltd", "50001121", "021000021", Savings, testAddress)
	if checking.Key == savings.Key {
		t.Error("changing the account category must change the key")
	}
}

func TestNewBeneficiary_NormalisesStateToTwoLetters(t *testing.T) {
	for _, in := range []string{"WA", "wa", " US-WA ", "us-wa"} {
		addr := testAddress
		addr.State = in
		b, err := NewBeneficiary("Acme Ltd", "50001121", "021000021", Checking, addr)
		if err != nil {
			t.Fatalf("state %q: unexpected error: %v", in, err)
		}
		if b.Address.State != "WA" {
			t.Errorf("state %q normalised to %q, want WA", in, b.Address.State)
		}
		if b.Key != mustBeneficiary(t, "Acme Ltd", "50001121", "021000021", testAddress).Key {
			t.Errorf("state %q keyed differently from WA", in)
		}
	}
}

func TestNewBeneficiary_RejectsInvalidUSAddressFormats(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*BeneficiaryAddress)
		wantErr string
	}{
		{"state spelled out", func(a *BeneficiaryAddress) { a.State = "Washington" }, "state"},
		{"state with digits", func(a *BeneficiaryAddress) { a.State = "W1" }, "state"},
		{"short postcode", func(a *BeneficiaryAddress) { a.Postcode = "9810" }, "postcode"},
		{"letters in postcode", func(a *BeneficiaryAddress) { a.Postcode = "ABCDE" }, "postcode"},
		{"street too short", func(a *BeneficiaryAddress) { a.Street = "1 A" }, "street"},
		{"street all digits", func(a *BeneficiaryAddress) { a.Street = "123456" }, "street"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := testAddress
			tt.mutate(&addr)
			_, err := NewBeneficiary("Acme Ltd", "50001121", "021000021", Checking, addr)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want an error mentioning %q, got %v", tt.wantErr, err)
			}
		})
	}

	addr := testAddress
	addr.Postcode = "98104-1234"
	if _, err := NewBeneficiary("Acme Ltd", "50001121", "021000021", Checking, addr); err != nil {
		t.Errorf("ZIP+4 postcode rejected: %v", err)
	}
}

func TestNewBeneficiary_RejectsNameTheRailWouldRefuse(t *testing.T) {
	for _, name := range []string{"A", "12345"} {
		if _, err := NewBeneficiary(name, "50001121", "021000021", Checking, testAddress); err == nil || !strings.Contains(err.Error(), "name") {
			t.Errorf("name %q: want a name error, got %v", name, err)
		}
	}
}
