package httpapi

import (
	"net/http"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

type policyRuleDTO struct {
	Currency               string `json:"currency"`
	MaxTransactionMinor    int64  `json:"max_transaction_minor"`
	Max24hMinor            int64  `json:"max_24h_minor"`
	Max7dMinor             int64  `json:"max_7d_minor"`
	Max30dMinor            int64  `json:"max_30d_minor"`
	MaxPerHourCount        int    `json:"max_per_hour_count"`
	ApprovalThresholdMinor int64  `json:"approval_threshold_minor"`
}

type policyVersionDTO struct {
	Version       int             `json:"version"`
	CurrencyRules []policyRuleDTO `json:"currency_rules"`
	PayeeAllow    []string        `json:"payee_allow"`
	PayeeBlock    []string        `json:"payee_block"`
	CategoryBlock []string        `json:"category_block"`
	CreatedAt     time.Time       `json:"created_at"`
	CreatedBy     string          `json:"created_by,omitempty"`
}

func newPolicyVersionDTO(v policy.PolicyVersion) policyVersionDTO {
	rules := make([]policyRuleDTO, 0, len(v.CurrencyRules))
	for c, r := range v.CurrencyRules {
		rules = append(rules, policyRuleDTO{
			Currency: string(c), MaxTransactionMinor: r.MaxPerTransactionMinor,
			Max24hMinor: r.Max24hMinor, Max7dMinor: r.Max7dMinor, Max30dMinor: r.Max30dMinor,
			MaxPerHourCount: r.MaxPerHourCount, ApprovalThresholdMinor: r.ApprovalThresholdMinor,
		})
	}
	return policyVersionDTO{
		Version: v.Version, CurrencyRules: rules, PayeeAllow: emptyIfNil(v.PayeeAllow),
		PayeeBlock: emptyIfNil(v.PayeeBlock), CategoryBlock: emptyIfNil(v.CategoryBlock),
		CreatedAt: v.CreatedAt.UTC(), CreatedBy: string(v.CreatedBy),
	}
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func rulesFromDTO(in []policyRuleDTO) map[domain.Currency]policy.CurrencyRule {
	out := make(map[domain.Currency]policy.CurrencyRule, len(in))
	for _, r := range in {
		out[domain.Currency(r.Currency)] = policy.CurrencyRule{
			MaxPerTransactionMinor: r.MaxTransactionMinor, Max24hMinor: r.Max24hMinor,
			Max7dMinor: r.Max7dMinor, Max30dMinor: r.Max30dMinor,
			MaxPerHourCount: r.MaxPerHourCount, ApprovalThresholdMinor: r.ApprovalThresholdMinor,
		}
	}
	return out
}

func (h *Handler) getCurrentPolicy(w http.ResponseWriter, r *http.Request) {
	v, err := h.GetCurrentPolicy.Run(r.Context(), userFrom(r.Context()).Organisation.ID)
	if err != nil {
		writeAppError(w, err, "could not load the organisation's policy")
		return
	}
	writeJSON(w, http.StatusOK, newPolicyVersionDTO(v))
}

func (h *Handler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrencyRules []policyRuleDTO `json:"currency_rules"`
		PayeeAllow    []string        `json:"payee_allow"`
		PayeeBlock    []string        `json:"payee_block"`
		CategoryBlock []string        `json:"category_block"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	saved, err := h.UpdatePolicy.Run(r.Context(), userFrom(r.Context()).Organisation.ID, app.UpdatePolicyCommand{
		CurrencyRules: rulesFromDTO(req.CurrencyRules), PayeeAllow: req.PayeeAllow, PayeeBlock: req.PayeeBlock, CategoryBlock: req.CategoryBlock,
	})
	if err != nil {
		writeAppError(w, err, "could not update the organisation's policy")
		return
	}
	writeJSON(w, http.StatusOK, newPolicyVersionDTO(saved))
}

type reasonDTO struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type policyDecisionDTO struct {
	Outcome       string      `json:"outcome"`
	Reasons       []reasonDTO `json:"reasons"`
	PolicyVersion int         `json:"policy_version"`
}

func newPolicyDecisionDTO(d policy.Decision) policyDecisionDTO {
	reasons := make([]reasonDTO, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		reasons = append(reasons, reasonDTO{Code: string(r.Code), Detail: r.Detail})
	}
	return policyDecisionDTO{Outcome: d.Outcome.String(), Reasons: reasons, PolicyVersion: d.Version}
}

func (h *Handler) dryRunPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Context     string   `json:"context"`
		Currency    string   `json:"currency"`
		AmountMinor int64    `json:"amount_minor"`
		Payee       *string  `json:"payee"`
		Categories  []string `json:"categories"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx, err := policy.ParseContext(req.Context)
	if err != nil {
		writeAppError(w, &app.ValidationError{Field: "context", Message: err.Error()}, "")
		return
	}
	if req.AmountMinor <= 0 {
		writeAppError(w, &app.ValidationError{Field: "amount_minor", Message: "amount_minor must be positive"}, "")
		return
	}
	d, err := h.DryRunPolicy.Run(r.Context(), userFrom(r.Context()).Organisation.ID, app.DryRunPolicyCommand{
		Context: ctx, Currency: domain.Currency(req.Currency), AmountMinor: req.AmountMinor, Payee: req.Payee, Categories: req.Categories,
	})
	if err != nil {
		writeAppError(w, err, "could not evaluate the candidate payment")
		return
	}
	writeJSON(w, http.StatusOK, newPolicyDecisionDTO(d))
}
