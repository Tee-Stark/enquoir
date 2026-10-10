package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Currency is an ISO 4217 code. Only currencies Enquoir has been told to
// support are valid.
type Currency string

const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
)

func (c Currency) valid() bool {
	switch c {
	case USD, EUR, GBP:
		return true
	default:
		return false
	}
}

// Money is an amount in integer minor units (e.g. cents) of a supported
// currency. There is no float anywhere in its representation.
type Money struct {
	minorUnits int64
	currency   Currency
}

// NewMoney builds a Money from a minor-unit amount. The amount must be
// strictly positive and the currency must be one Enquoir supports.
func NewMoney(minorUnits int64, currency Currency) (Money, error) {
	if !currency.valid() {
		return Money{}, fmt.Errorf("unsupported currency %q", currency)
	}
	if minorUnits <= 0 {
		return Money{}, fmt.Errorf("amount must be positive, got %d minor units", minorUnits)
	}
	return Money{minorUnits: minorUnits, currency: currency}, nil
}

func (m Money) MinorUnits() int64  { return m.minorUnits }
func (m Money) Currency() Currency { return m.currency }

// moneyJSON is Money's wire shape. Money's fields are unexported for
// good reason (every instance must pass through NewMoney's checks),
// but that also hides them from the default JSON marshaller - anything
// that serialises Money, including Temporal's data converter for a
// workflow or activity payload, would otherwise see nothing and
// silently produce "{}".
type moneyJSON struct {
	MinorUnits int64    `json:"minor_units"`
	Currency   Currency `json:"currency"`
}

func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{MinorUnits: m.minorUnits, Currency: m.currency})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var dto moneyJSON
	if err := json.Unmarshal(data, &dto); err != nil {
		return err
	}
	decoded, err := NewMoney(dto.MinorUnits, dto.Currency)
	if err != nil {
		return err
	}
	*m = decoded
	return nil
}

// ParseMajorUnits parses a decimal major-unit string such as "100.50" into
// Money, without ever going through a float. The fractional part, if
// present, must be one or two digits.
func ParseMajorUnits(decimal string, currency Currency) (Money, error) {
	if decimal == "" {
		return Money{}, errors.New("empty amount")
	}

	negative := strings.HasPrefix(decimal, "-")
	unsigned := strings.TrimPrefix(decimal, "-")

	whole, frac, hasFrac := strings.Cut(unsigned, ".")
	if whole == "" {
		return Money{}, fmt.Errorf("invalid amount %q: missing whole part", decimal)
	}
	if hasFrac {
		if len(frac) == 0 || len(frac) > 2 {
			return Money{}, fmt.Errorf("invalid amount %q: fractional part must be one or two digits", decimal)
		}
		if len(frac) == 1 {
			frac += "0"
		}
	} else {
		frac = "00"
	}

	wholeUnits, err := strconv.ParseInt(whole, 10, 63)
	if err != nil {
		return Money{}, fmt.Errorf("invalid amount %q: %w", decimal, err)
	}
	fracUnits, err := strconv.ParseInt(frac, 10, 63)
	if err != nil {
		return Money{}, fmt.Errorf("invalid amount %q: %w", decimal, err)
	}

	minorUnits := wholeUnits*100 + fracUnits
	if negative {
		minorUnits = -minorUnits
	}

	return NewMoney(minorUnits, currency)
}

// MajorUnitsString renders Money as a major-unit decimal string, e.g.
// "100.50", with no float involved.
func (m Money) MajorUnitsString() string {
	whole := m.minorUnits / 100
	frac := m.minorUnits % 100
	return fmt.Sprintf("%d.%02d", whole, frac)
}
