package commerce

import (
	"strings"
	"testing"
)

func TestNewFundingReference(t *testing.T) {
	tests := []struct {
		in, want, wantErr string
	}{
		{in: "mfm_123", want: "mfm_123"},
		{in: " mfm_123 ", want: "mfm_123"},
		{in: "pm_1Nq2fPLkdIwHu7ixXyZ", want: "pm_1Nq2fPLkdIwHu7ixXyZ"},
		{in: "order-12345678901", want: "order-12345678901"}, // 11 digits in a row
		{in: "", wantErr: "required"},
		{in: "4242424242424242", wantErr: "card number"},
		{in: "4242-4242-4242-4242", wantErr: "card number"},
		{in: "card_4242424242424242_x", wantErr: "card number"},
		{in: "123456789012", wantErr: "card number"}, // 12 digits
		{in: "has space", wantErr: "spaces"},
	}
	for _, tc := range tests {
		got, err := NewFundingReference(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewFundingReference(%q) = %q, %v; want an error containing %q", tc.in, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || string(got) != tc.want {
			t.Errorf("NewFundingReference(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestNewProvider(t *testing.T) {
	for in, want := range map[string]string{"stripe": "stripe", " Adyen ": "adyen", "checkout_com": "checkout_com", "mock-psp.v2": "mock-psp.v2"} {
		if got, err := NewProvider(in); err != nil || string(got) != want {
			t.Errorf("NewProvider(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a b", "_stripe", "stripe!", strings.Repeat("a", 41), "psp4242424242424242"} {
		if got, err := NewProvider(in); err == nil {
			t.Errorf("NewProvider(%q) = %q; want an error", in, got)
		}
	}
}

func TestNewFundingDisplay(t *testing.T) {
	good := []struct {
		brand, last4 string
		want         FundingDisplay
	}{
		{"visa", "4242", FundingDisplay{Brand: "visa", LastFour: "4242"}},
		{" Mastercard ", "0005", FundingDisplay{Brand: "mastercard", LastFour: "0005"}},
		{"american_express", "1000", FundingDisplay{Brand: "american_express", LastFour: "1000"}},
		{"paypal", "", FundingDisplay{Brand: "paypal"}}, // no last four for a wallet
	}
	for _, tc := range good {
		got, err := NewFundingDisplay(tc.brand, tc.last4)
		if err != nil || got != tc.want {
			t.Errorf("NewFundingDisplay(%q, %q) = %+v, %v; want %+v", tc.brand, tc.last4, got, err, tc.want)
		}
	}
	bad := []struct{ brand, last4, field string }{
		{"", "4242", "brand"},
		{"visa1", "4242", "brand"},
		{"visa card", "4242", "brand"},
		{strings.Repeat("v", 31), "4242", "brand"},
		{"visa", "424", "last_four"},
		{"visa", "42424", "last_four"},
		{"visa", "4242424242424242", "last_four"},
		{"visa", "42a2", "last_four"},
	}
	for _, tc := range bad {
		_, err := NewFundingDisplay(tc.brand, tc.last4)
		var ferr *FieldError
		if !asFieldError(err, &ferr) || ferr.Field != tc.field {
			t.Errorf("NewFundingDisplay(%q, %q): %v; want an error on %s", tc.brand, tc.last4, err, tc.field)
		}
	}
}

func asFieldError(err error, target **FieldError) bool {
	fe, ok := err.(*FieldError)
	if ok {
		*target = fe
	}
	return ok
}
