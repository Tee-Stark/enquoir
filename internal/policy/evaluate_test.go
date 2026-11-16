package policy_test

import (
	"testing"

	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

func rule() policy.CurrencyRule {
	return policy.CurrencyRule{
		MaxPerTransactionMinor: 10_000,
		Max24hMinor:            50_000,
		Max7dMinor:             200_000,
		Max30dMinor:            500_000,
		MaxPerHourCount:        5,
		ApprovalThresholdMinor: 8_000,
	}
}

func versionWith(t *testing.T, r policy.CurrencyRule, payeeAllow, payeeBlock, categoryBlock []string) policy.PolicyVersion {
	t.Helper()
	v, err := policy.NewPolicyVersion(domain.OrgID("org_a"), 1,
		map[domain.Currency]policy.CurrencyRule{domain.USD: r}, payeeAllow, payeeBlock, categoryBlock)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func hasReason(d policy.Decision, code policy.ReasonCode) bool {
	for _, r := range d.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func TestEvaluate_AllowsWithinEveryLimit(t *testing.T) {
	v := versionWith(t, rule(), nil, nil, nil)
	d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.USD, AmountMinor: 1_000}, policy.Usage{})
	if d.Outcome != policy.Allow {
		t.Errorf("outcome = %v, reasons = %+v, want Allow", d.Outcome, d.Reasons)
	}
}

func TestEvaluate_DeniesOverPerTransactionLimit_WithCorrectReasonCode(t *testing.T) {
	v := versionWith(t, rule(), nil, nil, nil)
	d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.USD, AmountMinor: 20_000}, policy.Usage{})
	if d.Outcome != policy.Deny || !hasReason(d, policy.ReasonOverTransactionLimit) {
		t.Errorf("decision = %+v, want Deny with ReasonOverTransactionLimit", d)
	}
}

func TestEvaluate_AccumulatesEveryFailingReason_NotJustFirst(t *testing.T) {
	v := versionWith(t, rule(), nil, nil, nil)
	// Over the per-transaction limit AND the rolling-24h total AND the hourly count, all at once.
	d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.USD, AmountMinor: 60_000},
		policy.Usage{Last24h: 40_000, LastHourCount: 5})
	for _, code := range []policy.ReasonCode{policy.ReasonOverTransactionLimit, policy.ReasonOver24hTotal, policy.ReasonOverHourlyCount} {
		if !hasReason(d, code) {
			t.Errorf("decision = %+v, missing reason %s", d, code)
		}
	}
	if d.Outcome != policy.Deny {
		t.Errorf("outcome = %v, want Deny", d.Outcome)
	}
}

func TestEvaluate_DenyBeatsEscalate_WhenBothWouldApply(t *testing.T) {
	v := versionWith(t, rule(), nil, nil, nil)
	// Over both the per-transaction limit (deny) and the approval threshold (would escalate alone).
	d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.USD, AmountMinor: 20_000}, policy.Usage{})
	if d.Outcome != policy.Deny {
		t.Errorf("outcome = %v, want Deny (deny beats escalate)", d.Outcome)
	}
}

func TestEvaluate_EscalatesOnlyAboveThreshold_NothingElseFailing(t *testing.T) {
	v := versionWith(t, rule(), nil, nil, nil)
	// 9,000 is over the 8,000 threshold but under the 10,000 transaction limit and every other limit.
	d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.USD, AmountMinor: 9_000}, policy.Usage{})
	if d.Outcome != policy.Escalate {
		t.Errorf("decision = %+v, want Escalate", d)
	}
}

func TestEvaluate_TreasuryPayeeBlockApplies_CommerceCategoryBlockDoesNot(t *testing.T) {
	v := versionWith(t, rule(), nil, []string{"pay_evil"}, []string{"electronics"})
	payee := "pay_evil"
	d := policy.Evaluate(v, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 1_000, Payee: &payee,
	}, policy.Usage{})
	if d.Outcome != policy.Deny || !hasReason(d, policy.ReasonPayeeBlocked) {
		t.Errorf("treasury payee-block: decision = %+v, want Deny/ReasonPayeeBlocked", d)
	}

	// The same payee, outside treasury context, is irrelevant - only category block applies in commerce.
	d2 := policy.Evaluate(v, policy.EvaluationInput{
		Context: policy.ContextCommerce, Currency: domain.USD, AmountMinor: 1_000, Categories: []string{"books"},
	}, policy.Usage{})
	if d2.Outcome != policy.Allow {
		t.Errorf("commerce with an unrelated category: decision = %+v, want Allow", d2)
	}
}

func TestEvaluate_CommerceCategoryBlockApplies_TreasuryPayeeAllowDoesNot(t *testing.T) {
	v := versionWith(t, rule(), nil, nil, []string{"electronics"})
	d := policy.Evaluate(v, policy.EvaluationInput{
		Context: policy.ContextCommerce, Currency: domain.USD, AmountMinor: 1_000, Categories: []string{"electronics"},
	}, policy.Usage{})
	if d.Outcome != policy.Deny || !hasReason(d, policy.ReasonCategoryBlocked) {
		t.Errorf("commerce category-block: decision = %+v, want Deny/ReasonCategoryBlocked", d)
	}
}

func TestEvaluate_PayeeAllowList_BlocksAnyoneNotOnIt(t *testing.T) {
	v := versionWith(t, rule(), []string{"pay_friend"}, nil, nil)
	stranger := "pay_stranger"
	d := policy.Evaluate(v, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 1_000, Payee: &stranger,
	}, policy.Usage{})
	if d.Outcome != policy.Deny || !hasReason(d, policy.ReasonPayeeNotAllowed) {
		t.Errorf("payee not on allow list: decision = %+v, want Deny/ReasonPayeeNotAllowed", d)
	}

	friend := "pay_friend"
	d2 := policy.Evaluate(v, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 1_000, Payee: &friend,
	}, policy.Usage{})
	if d2.Outcome != policy.Allow {
		t.Errorf("payee on allow list: decision = %+v, want Allow", d2)
	}
}

func TestEvaluate_NoRuleForCurrency_Denies(t *testing.T) {
	// The dense model always fills every supported currency, but Evaluate
	// itself must still fail safe (deny, not panic/allow) if ever handed a
	// version missing one - e.g. a future currency not yet in domain.Currency.
	v := versionWith(t, rule(), nil, nil, nil)
	delete(v.CurrencyRules, domain.EUR)
	d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.EUR, AmountMinor: 1_000}, policy.Usage{})
	if d.Outcome != policy.Deny || !hasReason(d, policy.ReasonNoCurrencyRule) {
		t.Errorf("decision = %+v, want Deny/ReasonNoCurrencyRule", d)
	}
}

func TestParseContext(t *testing.T) {
	for in, want := range map[string]policy.Context{"treasury": policy.ContextTreasury, "commerce": policy.ContextCommerce} {
		if got, err := policy.ParseContext(in); err != nil || got != want {
			t.Errorf("ParseContext(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := policy.ParseContext("comerce"); err == nil {
		t.Error("ParseContext of a typo: want an error, not a silent default")
	}
}

func TestEvaluate_RollingTotals_EachWindowCheckedIndependently(t *testing.T) {
	cases := []struct {
		name  string
		usage policy.Usage
		want  policy.ReasonCode
	}{
		{"24h", policy.Usage{Last24h: 45_000}, policy.ReasonOver24hTotal},
		{"7d", policy.Usage{Last7d: 195_000}, policy.ReasonOver7dTotal},
		{"30d", policy.Usage{Last30d: 495_000}, policy.ReasonOver30dTotal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := versionWith(t, rule(), nil, nil, nil)
			d := policy.Evaluate(v, policy.EvaluationInput{Currency: domain.USD, AmountMinor: 6_000}, c.usage)
			if d.Outcome != policy.Deny || !hasReason(d, c.want) {
				t.Errorf("decision = %+v, want Deny/%s", d, c.want)
			}
		})
	}
}
