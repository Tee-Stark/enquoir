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
// this reference, for this reason, on behalf of one organisation.
type Intent struct {
	OrgID      OrgID
	TreasuryID TreasuryID
	Payee      string
	Amount     Money
	Reference  string
	Reason     string
	Key        IntentKey

	// Agent is the agent that submitted the intent. Recorded, not part
	// of the key: two agents of one organisation paying the same
	// invoice are the same payment.
	Agent AgentID

	// PolicyVersion is the policy version that decided this intent at
	// submission (Allow or Escalate - a Deny is never saved), nil when
	// no Policies store was configured for the submission. AGENTS.md's
	// Phase 6 requires every decision to record the version it used.
	PolicyVersion *int
}

// NewIntent builds an Intent, normalising its reference (trimmed) and
// currency (upper-cased) before deriving its key. The reason is kept on
// the Intent for the record but is not part of its key: it is free text
// an agent writes, and a reworded retry is still the same payment.
//
// Tenants are isolated: the organisation and the treasury are part of
// the key, so deduplication is per organisation and treasury, never
// across them. The key is also the rail's request_id, so it stays
// unique across the one shared rail account.
func NewIntent(org OrgID, treasury TreasuryID, payee string, minorUnits int64, currency, reference, reason string) (Intent, error) {
	if strings.TrimSpace(string(org)) == "" {
		return Intent{}, errors.New("organisation is required")
	}
	if strings.TrimSpace(string(treasury)) == "" {
		return Intent{}, errors.New("treasury is required")
	}
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
		OrgID: org, TreasuryID: treasury,
		Payee:     payee,
		Amount:    amount,
		Reference: reference,
		Reason:    reason,
		Key:       deriveIntentKey(org, treasury, payee, amount, reference),
	}, nil
}

// LegacyKey is the key this intent had before treasuries: without the
// treasury. An intent recorded then keeps that key; a resubmission must
// find it, not derive a new key and pay again (app.SubmitIntent checks).
func (i Intent) LegacyKey() IntentKey {
	return hashKey(string(i.OrgID), i.Payee, strconv.FormatInt(i.Amount.MinorUnits(), 10), string(i.Amount.Currency()), i.Reference)
}

// deriveIntentKey hashes the fields that identify a payment - which
// organisation and treasury, who is paid, how much, in what currency,
// against which reference - with a separator byte between them so no
// combination of field values can collide by shifting a boundary.
func deriveIntentKey(org OrgID, treasury TreasuryID, payee string, amount Money, reference string) IntentKey {
	return hashKey(string(org), string(treasury), payee, strconv.FormatInt(amount.MinorUnits(), 10), string(amount.Currency()), reference)
}

func hashKey(fields ...string) IntentKey {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return IntentKey(hex.EncodeToString(sum[:])[:32])
}
