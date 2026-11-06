package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// IntentKey is a stable, deterministic key derived from an Intent's fields.
// Two intents that mean the same payment produce the same key, which is
// what lets Enquoir recognise a re-sent intent instead of paying it twice.
type IntentKey string

// Intent states a single payment to make: pay this payee this amount for
// this reference, for this reason.
type Intent struct {
	Payee     string
	Amount    Money
	Reference string
	Reason    string
	Key       IntentKey
}

// NewIntent builds an Intent, normalising its reference (trimmed) and
// currency (upper-cased) before deriving its key. The reason is kept on
// the Intent for the record but is not part of its key: it is free text
// an agent writes, and a reworded retry is still the same payment.
func NewIntent(payee string, minorUnits int64, currency, reference, reason string) (Intent, error) {
	if strings.TrimSpace(payee) == "" {
		return Intent{}, errors.New("payee is required")
	}

	reference = strings.TrimSpace(reference)
	if reference == "" {
		return Intent{}, errors.New("reference is required")
	}

	amount, err := NewMoney(minorUnits, Currency(strings.ToUpper(strings.TrimSpace(currency))))
	if err != nil {
		return Intent{}, err
	}

	return Intent{
		Payee:     payee,
		Amount:    amount,
		Reference: reference,
		Reason:    reason,
		Key:       deriveIntentKey(payee, amount, reference),
	}, nil
}

// deriveIntentKey hashes the fields that identify a payment - who is
// paid, how much, in what currency, against which reference - with a
// separator byte between them so no combination of field values can
// collide by shifting a boundary.
func deriveIntentKey(payee string, amount Money, reference string) IntentKey {
	const sep = "\x00"
	canonical := strings.Join([]string{
		payee,
		strconv.FormatInt(amount.MinorUnits(), 10),
		string(amount.Currency()),
		reference,
	}, sep)

	sum := sha256.Sum256([]byte(canonical))
	return IntentKey(hex.EncodeToString(sum[:])[:32])
}
