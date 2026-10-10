package domain

import (
	"regexp"
	"testing"
)

func mustIntent(t *testing.T, payee string, minorUnits int64, currency, reference, reason string) Intent {
	t.Helper()
	intent, err := NewIntent(payee, minorUnits, currency, reference, reason)
	if err != nil {
		t.Fatalf("NewIntent(%q, %d, %q, %q, %q): unexpected error: %v", payee, minorUnits, currency, reference, reason, err)
	}
	return intent
}

func TestIntentKey_SameFieldsGiveSameKey(t *testing.T) {
	a := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "invoice settlement")
	b := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "invoice settlement")
	if a.Key != b.Key {
		t.Errorf("identical intents produced different keys: %q vs %q", a.Key, b.Key)
	}
}

func TestIntentKey_ChangingAnyFieldChangesKey(t *testing.T) {
	base := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "invoice settlement")

	variants := []Intent{
		mustIntent(t, "other-payee", 10050, "USD", "INV-1001", "invoice settlement"),
		mustIntent(t, "acme-ltd", 999, "USD", "INV-1001", "invoice settlement"),
		mustIntent(t, "acme-ltd", 10050, "EUR", "INV-1001", "invoice settlement"),
		mustIntent(t, "acme-ltd", 10050, "USD", "INV-1002", "invoice settlement"),
		mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "different reason"),
	}

	for i, v := range variants {
		if v.Key == base.Key {
			t.Errorf("variant %d: expected a different key from base, got the same %q", i, v.Key)
		}
	}
}

func TestIntentKey_DistinctReasonChangesKey(t *testing.T) {
	a := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "first run")
	b := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "retry after timeout")
	if a.Key == b.Key {
		t.Errorf("distinct reasons produced the same key: %q", a.Key)
	}
}

func TestIntentKey_NormalisesReferenceAndCurrency(t *testing.T) {
	canonical := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "invoice settlement")
	padded := mustIntent(t, "acme-ltd", 10050, "usd", "  INV-1001  ", "invoice settlement")

	if canonical.Key != padded.Key {
		t.Errorf("normalised inputs should key the same: %q vs %q", canonical.Key, padded.Key)
	}
	if padded.Reference != "INV-1001" {
		t.Errorf("Reference not trimmed: got %q", padded.Reference)
	}
	if padded.Amount.Currency() != USD {
		t.Errorf("Currency not upper-cased: got %q", padded.Amount.Currency())
	}
}

var safeKeyCharset = regexp.MustCompile(`^[a-z0-9]+$`)

func TestIntentKey_IsSafeLengthAndCharset(t *testing.T) {
	intent := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "invoice settlement")
	key := string(intent.Key)

	if len(key) < 10 || len(key) > 50 {
		t.Errorf("key length %d outside [10, 50]: %q", len(key), key)
	}
	if !safeKeyCharset.MatchString(key) {
		t.Errorf("key contains unsafe characters: %q", key)
	}
}

// Golden: pins a known key for a fixed input so an accidental change to the
// derivation algorithm is caught by a test, not discovered in production.
func TestIntentKey_Golden(t *testing.T) {
	intent := mustIntent(t, "acme-ltd", 10050, "USD", "INV-1001", "invoice settlement")
	const want = "411962bef49df9842be394ab137286a6"
	if string(intent.Key) != want {
		t.Errorf("golden key changed: got %q, want %q", intent.Key, want)
	}
}

func TestNewIntent_RejectsEmptyPayeeOrReference(t *testing.T) {
	if _, err := NewIntent("", 10050, "USD", "INV-1001", "reason"); err == nil {
		t.Error("NewIntent with empty payee: want error, got nil")
	}
	if _, err := NewIntent("acme-ltd", 10050, "USD", "   ", "reason"); err == nil {
		t.Error("NewIntent with blank reference: want error, got nil")
	}
}

func TestNewIntent_RejectsInvalidMoney(t *testing.T) {
	if _, err := NewIntent("acme-ltd", 0, "USD", "INV-1001", "reason"); err == nil {
		t.Error("NewIntent with zero amount: want error, got nil")
	}
	if _, err := NewIntent("acme-ltd", 10050, "JPY", "INV-1001", "reason"); err == nil {
		t.Error("NewIntent with unsupported currency: want error, got nil")
	}
}
