package policy_test

import (
	"testing"

	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

func testVersion(t *testing.T) policy.PolicyVersion {
	t.Helper()
	v, err := policy.NewPolicyVersion(domain.OrgID("org_a"), 1, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {
			MaxPerTransactionMinor: 10_000,
			Max24hMinor:            50_000,
			Max7dMinor:             200_000,
			Max30dMinor:            500_000,
			MaxPerHourCount:        5,
			ApprovalThresholdMinor: 8_000,
		},
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNewPolicyVersion_FillsEveryCurrencyRuleDensely(t *testing.T) {
	v := testVersion(t)
	for _, c := range []domain.Currency{domain.USD, domain.EUR, domain.GBP} {
		if _, ok := v.CurrencyRules[c]; !ok {
			t.Errorf("missing a rule for %s; every supported currency must always have one", c)
		}
	}
	// EUR/GBP were never specified, so they must default to unlimited (zero value).
	if r := v.CurrencyRules[domain.EUR]; r.MaxPerTransactionMinor != 0 || r.ApprovalThresholdMinor != 0 {
		t.Errorf("unspecified currency rule = %+v, want the zero/unlimited value", r)
	}
}

func TestPolicyVersion_CanonicalIsDeterministic(t *testing.T) {
	a, b := testVersion(t), testVersion(t)
	if string(a.Canonical()) != string(b.Canonical()) {
		t.Error("two equally-built versions produced different canonical forms")
	}
}

func TestPolicyVersion_HashChangesWithAnyField(t *testing.T) {
	a := testVersion(t)
	b, err := policy.NewPolicyVersion(domain.OrgID("org_a"), 1, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {MaxPerTransactionMinor: 99_999},
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() == b.Hash() {
		t.Error("changing a currency rule's limit did not change the hash")
	}
}

func TestPolicyVersion_HashIgnoresListOrder(t *testing.T) {
	a, err := policy.NewPolicyVersion(domain.OrgID("org_a"), 1, nil, []string{"pay_1", "pay_2"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := policy.NewPolicyVersion(domain.OrgID("org_a"), 1, nil, []string{"pay_2", "pay_1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() != b.Hash() {
		t.Error("reordering the payee allow list changed the hash; lists must be canonicalised")
	}
}
