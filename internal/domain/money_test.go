package domain

import (
	"encoding/json"
	"testing"
)

func TestNewMoney_RejectsUnsupportedCurrency(t *testing.T) {
	cases := []Currency{"", "JPY", "usd", "XXX"}
	for _, c := range cases {
		if _, err := NewMoney(100, c); err == nil {
			t.Errorf("NewMoney(100, %q): want error, got nil", c)
		}
	}
}

func TestNewMoney_AcceptsSupportedCurrencies(t *testing.T) {
	for _, c := range []Currency{USD, EUR, GBP} {
		if _, err := NewMoney(100, c); err != nil {
			t.Errorf("NewMoney(100, %q): unexpected error: %v", c, err)
		}
	}
}

func TestNewMoney_RejectsZeroAndNegativeAmounts(t *testing.T) {
	for _, amount := range []int64{0, -1, -10050} {
		if _, err := NewMoney(amount, USD); err == nil {
			t.Errorf("NewMoney(%d, USD): want error, got nil", amount)
		}
	}
}

func TestParseMajorUnits_ConvertsDecimalStringToMinorUnits(t *testing.T) {
	cases := []struct {
		decimal string
		want    int64
	}{
		{"100.50", 10050},
		{"0.01", 1},
		{"100", 10000},
		{"100.5", 10050},
		{"1.00", 100},
	}
	for _, tc := range cases {
		m, err := ParseMajorUnits(tc.decimal, USD)
		if err != nil {
			t.Errorf("ParseMajorUnits(%q): unexpected error: %v", tc.decimal, err)
			continue
		}
		if m.MinorUnits() != tc.want {
			t.Errorf("ParseMajorUnits(%q).MinorUnits() = %d, want %d", tc.decimal, m.MinorUnits(), tc.want)
		}
	}
}

func TestParseMajorUnits_RejectsMalformedInput(t *testing.T) {
	cases := []string{"", "abc", "100.", "100.555", "-5.00", "1.2.3", ".50", "100.5a"}
	for _, decimal := range cases {
		if _, err := ParseMajorUnits(decimal, USD); err == nil {
			t.Errorf("ParseMajorUnits(%q): want error, got nil", decimal)
		}
	}
}

func TestParseMajorUnits_RejectsZeroAndNegative(t *testing.T) {
	cases := []string{"0", "0.00", "-1.00"}
	for _, decimal := range cases {
		if _, err := ParseMajorUnits(decimal, USD); err == nil {
			t.Errorf("ParseMajorUnits(%q): want error, got nil", decimal)
		}
	}
}

func TestMoney_MajorUnitsString_FormatsWithTwoDecimals(t *testing.T) {
	cases := []struct {
		minorUnits int64
		want       string
	}{
		{10050, "100.50"},
		{1, "0.01"},
		{10000, "100.00"},
		{100, "1.00"},
	}
	for _, tc := range cases {
		m, err := NewMoney(tc.minorUnits, USD)
		if err != nil {
			t.Fatalf("NewMoney(%d, USD): unexpected error: %v", tc.minorUnits, err)
		}
		if got := m.MajorUnitsString(); got != tc.want {
			t.Errorf("Money{%d}.MajorUnitsString() = %q, want %q", tc.minorUnits, got, tc.want)
		}
	}
}

func TestMoney_RoundTripsThroughMajorUnitsString(t *testing.T) {
	decimals := []string{"100.50", "0.01", "100.00", "1.00"}
	for _, decimal := range decimals {
		m, err := ParseMajorUnits(decimal, EUR)
		if err != nil {
			t.Fatalf("ParseMajorUnits(%q): unexpected error: %v", decimal, err)
		}
		if got := m.MajorUnitsString(); got != decimal {
			t.Errorf("round trip for %q: got %q", decimal, got)
		}
	}
}

// Money's fields are unexported, but it still crosses JSON boundaries
// (e.g. Temporal's data converter, for any workflow/activity input or
// output that embeds it) - the default struct marshalling would see
// nothing and silently produce "{}". This must round-trip for real.
func TestMoney_JSONRoundTrip(t *testing.T) {
	original, err := NewMoney(10050, USD)
	if err != nil {
		t.Fatalf("NewMoney: unexpected error: %v", err)
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: unexpected error: %v", err)
	}

	var decoded Money
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}

	if decoded.MinorUnits() != original.MinorUnits() || decoded.Currency() != original.Currency() {
		t.Errorf("round trip changed the value: got %+v, want %+v", decoded, original)
	}
}

func TestMoney_UnmarshalJSON_RejectsInvalidValue(t *testing.T) {
	var m Money
	if err := json.Unmarshal([]byte(`{"minor_units":0,"currency":"USD"}`), &m); err == nil {
		t.Error("unmarshalling a zero amount: want error, got nil")
	}
	if err := json.Unmarshal([]byte(`{"minor_units":100,"currency":"JPY"}`), &m); err == nil {
		t.Error("unmarshalling an unsupported currency: want error, got nil")
	}
}
