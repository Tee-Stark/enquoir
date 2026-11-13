package policy

import (
	"fmt"

	"enquoir/internal/domain"
)

// Context is which bounded context a candidate payment belongs to -
// it scopes which rules apply: payee lists are treasury-only, the
// category block list is commerce-only.
type Context int

const (
	ContextTreasury Context = iota
	ContextCommerce
)

// Outcome is a policy decision's result. Escalate is only reached
// when nothing already forced Deny, so deny beats escalate by
// construction.
type Outcome int

const (
	Allow Outcome = iota
	Deny
	Escalate
)

func (o Outcome) String() string {
	switch o {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	case Escalate:
		return "escalate"
	default:
		return "unknown"
	}
}

// ReasonCode is a stable, closed set of reasons a decision can carry.
type ReasonCode string

const (
	ReasonOverTransactionLimit  ReasonCode = "over_transaction_limit"
	ReasonOver24hTotal          ReasonCode = "over_24h_total"
	ReasonOver7dTotal           ReasonCode = "over_7d_total"
	ReasonOver30dTotal          ReasonCode = "over_30d_total"
	ReasonOverHourlyCount       ReasonCode = "over_hourly_count"
	ReasonPayeeBlocked          ReasonCode = "payee_blocked"
	ReasonPayeeNotAllowed       ReasonCode = "payee_not_allowed"
	ReasonCategoryBlocked       ReasonCode = "category_blocked"
	ReasonNoCurrencyRule        ReasonCode = "no_currency_rule"
	ReasonOverApprovalThreshold ReasonCode = "over_approval_threshold"
)

// Reason is one specific way a decision came out the way it did.
type Reason struct {
	Code   ReasonCode
	Detail string
}

// Decision is a policy evaluation's result: Allow, Deny (every
// failing reason, never just the first) or Escalate. Version is the
// PolicyVersion.Version it was decided against, for callers to record
// alongside the decision.
type Decision struct {
	Outcome Outcome
	Reasons []Reason
	Version int
}

// MinorTotal is a sum of minor-unit amounts over a rolling window.
type MinorTotal int64

// Usage is an organisation's rolling totals in one currency, as of
// the moment of evaluation - computed by the caller (from Postgres,
// under a lock), never by this package, which stays free of I/O.
type Usage struct {
	Last24h       MinorTotal
	Last7d        MinorTotal
	Last30d       MinorTotal
	LastHourCount int
}

// EvaluationInput is the candidate payment being decided.
type EvaluationInput struct {
	OrgID       domain.OrgID
	Context     Context
	Currency    domain.Currency
	AmountMinor int64
	// Payee is set only for treasury payments; nil elsewhere.
	Payee *string
	// Categories is set only for commerce checkouts; nil elsewhere.
	Categories []string
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Evaluate decides a candidate payment against one policy version and
// its organisation's current usage. Every check runs independently -
// a denial never short-circuits the rest, so Deny always carries
// every reason the payment failed, not just the first found.
func Evaluate(v PolicyVersion, in EvaluationInput, u Usage) Decision {
	rule, ok := v.CurrencyRules[in.Currency]
	if !ok {
		return Decision{
			Outcome: Deny,
			Reasons: []Reason{{Code: ReasonNoCurrencyRule, Detail: fmt.Sprintf("no policy rule for currency %s", in.Currency)}},
			Version: v.Version,
		}
	}

	var reasons []Reason
	add := func(code ReasonCode, format string, args ...any) {
		reasons = append(reasons, Reason{Code: code, Detail: fmt.Sprintf(format, args...)})
	}

	if rule.MaxPerTransactionMinor > 0 && in.AmountMinor > rule.MaxPerTransactionMinor {
		add(ReasonOverTransactionLimit, "amount %d exceeds the per-transaction limit of %d", in.AmountMinor, rule.MaxPerTransactionMinor)
	}
	if rule.Max24hMinor > 0 && int64(u.Last24h)+in.AmountMinor > rule.Max24hMinor {
		add(ReasonOver24hTotal, "amount %d would bring the rolling 24h total to %d, over the limit of %d", in.AmountMinor, int64(u.Last24h)+in.AmountMinor, rule.Max24hMinor)
	}
	if rule.Max7dMinor > 0 && int64(u.Last7d)+in.AmountMinor > rule.Max7dMinor {
		add(ReasonOver7dTotal, "amount %d would bring the rolling 7d total to %d, over the limit of %d", in.AmountMinor, int64(u.Last7d)+in.AmountMinor, rule.Max7dMinor)
	}
	if rule.Max30dMinor > 0 && int64(u.Last30d)+in.AmountMinor > rule.Max30dMinor {
		add(ReasonOver30dTotal, "amount %d would bring the rolling 30d total to %d, over the limit of %d", in.AmountMinor, int64(u.Last30d)+in.AmountMinor, rule.Max30dMinor)
	}
	if rule.MaxPerHourCount > 0 && u.LastHourCount+1 > rule.MaxPerHourCount {
		add(ReasonOverHourlyCount, "this would be payment %d in the last hour, over the limit of %d", u.LastHourCount+1, rule.MaxPerHourCount)
	}

	if in.Context == ContextTreasury && in.Payee != nil {
		if contains(v.PayeeBlock, *in.Payee) {
			add(ReasonPayeeBlocked, "payee %s is on the policy's block list", *in.Payee)
		}
		if len(v.PayeeAllow) > 0 && !contains(v.PayeeAllow, *in.Payee) {
			add(ReasonPayeeNotAllowed, "payee %s is not on the policy's allow list", *in.Payee)
		}
	}
	if in.Context == ContextCommerce {
		for _, cat := range in.Categories {
			if contains(v.CategoryBlock, cat) {
				add(ReasonCategoryBlocked, "category %s is on the policy's block list", cat)
			}
		}
	}

	if len(reasons) > 0 {
		return Decision{Outcome: Deny, Reasons: reasons, Version: v.Version}
	}

	if rule.ApprovalThresholdMinor > 0 && in.AmountMinor > rule.ApprovalThresholdMinor {
		return Decision{
			Outcome: Escalate,
			Reasons: []Reason{{Code: ReasonOverApprovalThreshold, Detail: fmt.Sprintf("amount %d exceeds the approval threshold of %d", in.AmountMinor, rule.ApprovalThresholdMinor)}},
			Version: v.Version,
		}
	}

	return Decision{Outcome: Allow, Version: v.Version}
}
