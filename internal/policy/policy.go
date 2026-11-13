// Package policy is the shared spending-policy engine both the
// treasury and commerce bounded contexts evaluate payments against.
// It imports only the standard library and internal/domain, so
// neither bounded context gains a dependency on the other by using it
// (internal/archtest's TestPolicyImportsOnlyStandardLibraryAndDomain
// guards this).
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"enquoir/internal/domain"
)

// supportedCurrencies is every currency a policy version must always
// carry a rule for, dense: a version never has a "missing" currency,
// only ones whose rule happens to be the zero/unlimited value.
var supportedCurrencies = []domain.Currency{domain.USD, domain.EUR, domain.GBP}

// CurrencyRule is one currency's limits within a policy version.
// Every numeric field is a ceiling; zero means unlimited, never "zero
// allowed" - there is no legitimate policy that blocks every payment
// in a currency while still listing a rule for it.
type CurrencyRule struct {
	MaxPerTransactionMinor int64
	Max24hMinor            int64
	Max7dMinor             int64
	Max30dMinor            int64
	MaxPerHourCount        int
	ApprovalThresholdMinor int64
}

// PolicyVersion is one organisation's spending policy at a point in
// time. Every edit creates a new version; nothing is ever mutated in
// place, so a past decision's recorded version always reads back
// exactly what it was decided against.
type PolicyVersion struct {
	OrgID         domain.OrgID
	Version       int
	CurrencyRules map[domain.Currency]CurrencyRule
	// PayeeAllow/PayeeBlock apply only in the treasury context; an
	// empty PayeeAllow means no allow-list restriction at all.
	PayeeAllow []string
	PayeeBlock []string
	// CategoryBlock applies only in the commerce context.
	CategoryBlock []string
	CreatedAt     time.Time
	CreatedBy     domain.UserID
}

// NewPolicyVersion builds a version with every supported currency
// densely present: an omitted currency in rules defaults to the
// zero/unlimited rule, rather than being absent.
func NewPolicyVersion(org domain.OrgID, version int, rules map[domain.Currency]CurrencyRule, payeeAllow, payeeBlock, categoryBlock []string) (PolicyVersion, error) {
	dense := make(map[domain.Currency]CurrencyRule, len(supportedCurrencies))
	for _, c := range supportedCurrencies {
		dense[c] = rules[c]
	}
	return PolicyVersion{
		OrgID:         org,
		Version:       version,
		CurrencyRules: dense,
		PayeeAllow:    payeeAllow,
		PayeeBlock:    payeeBlock,
		CategoryBlock: categoryBlock,
	}, nil
}

// canonicalRule mirrors CurrencyRule with an explicit currency field,
// for a deterministic JSON encoding.
type canonicalRule struct {
	Currency               string `json:"currency"`
	MaxPerTransactionMinor int64  `json:"max_transaction_minor"`
	Max24hMinor            int64  `json:"max_24h_minor"`
	Max7dMinor             int64  `json:"max_7d_minor"`
	Max30dMinor            int64  `json:"max_30d_minor"`
	MaxPerHourCount        int    `json:"max_per_hour_count"`
	ApprovalThresholdMinor int64  `json:"approval_threshold_minor"`
}

type canonicalVersion struct {
	Version       int             `json:"version"`
	OrgID         string          `json:"org_id"`
	CurrencyRules []canonicalRule `json:"currency_rules"`
	PayeeAllow    []string        `json:"payee_allow"`
	PayeeBlock    []string        `json:"payee_block"`
	CategoryBlock []string        `json:"category_block"`
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

// Canonical is the version's deterministic snapshot: what Hash is
// computed over, and what gets stored alongside the version.
func (v PolicyVersion) Canonical() []byte {
	rules := make([]canonicalRule, 0, len(v.CurrencyRules))
	for c, r := range v.CurrencyRules {
		rules = append(rules, canonicalRule{
			Currency: string(c), MaxPerTransactionMinor: r.MaxPerTransactionMinor,
			Max24hMinor: r.Max24hMinor, Max7dMinor: r.Max7dMinor, Max30dMinor: r.Max30dMinor,
			MaxPerHourCount: r.MaxPerHourCount, ApprovalThresholdMinor: r.ApprovalThresholdMinor,
		})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Currency < rules[j].Currency })

	b, err := json.Marshal(canonicalVersion{
		Version: v.Version, OrgID: string(v.OrgID), CurrencyRules: rules,
		PayeeAllow: sortedCopy(v.PayeeAllow), PayeeBlock: sortedCopy(v.PayeeBlock), CategoryBlock: sortedCopy(v.CategoryBlock),
	})
	if err != nil {
		panic("policy: marshalling policy version: " + err.Error()) // only strings and numbers: cannot fail
	}
	return b
}

// Hash is "sha256:" and the hex SHA-256 of the canonical snapshot.
func (v PolicyVersion) Hash() string {
	sum := sha256.Sum256(v.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}
