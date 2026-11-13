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
	"fmt"
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

// isSupportedCurrency reports whether c is one of supportedCurrencies.
func isSupportedCurrency(c domain.Currency) bool {
	for _, s := range supportedCurrencies {
		if c == s {
			return true
		}
	}
	return false
}

// validateRule rejects a negative ceiling - zero means unlimited,
// but a negative limit has no legitimate meaning and would otherwise
// be treated as "everything exceeds it", the opposite of what an
// owner editing a policy almost certainly intends.
func validateRule(c domain.Currency, r CurrencyRule) error {
	fields := map[string]int64{
		"max_transaction_minor":    r.MaxPerTransactionMinor,
		"max_24h_minor":            r.Max24hMinor,
		"max_7d_minor":             r.Max7dMinor,
		"max_30d_minor":            r.Max30dMinor,
		"max_per_hour_count":       int64(r.MaxPerHourCount),
		"approval_threshold_minor": r.ApprovalThresholdMinor,
	}
	for name, v := range fields {
		if v < 0 {
			return fmt.Errorf("currency %s: %s must not be negative, got %d", c, name, v)
		}
	}
	return nil
}

// NewPolicyVersion builds a version with every supported currency
// densely present: an omitted currency in rules defaults to the
// zero/unlimited rule, rather than being absent. It is an error for
// rules to name a currency this policy engine does not support, or a
// negative value for any limit - both are rejected here rather than
// silently dropped or saved, since a policy is a safety control, and
// a safety control must fail closed on bad input, not open.
func NewPolicyVersion(org domain.OrgID, version int, rules map[domain.Currency]CurrencyRule, payeeAllow, payeeBlock, categoryBlock []string) (PolicyVersion, error) {
	for c, r := range rules {
		if !isSupportedCurrency(c) {
			return PolicyVersion{}, fmt.Errorf("unsupported currency %q: must be one of %v", c, supportedCurrencies)
		}
		if err := validateRule(c, r); err != nil {
			return PolicyVersion{}, err
		}
	}
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

// ParsePolicyVersion reads a stored canonical snapshot back. It does
// not restore OrgID/Version as top-level typed fields beyond what the
// snapshot itself carries - the store, which knows which row this
// came from, sets those from its own columns after parsing.
func ParsePolicyVersion(b []byte) (PolicyVersion, error) {
	var c canonicalVersion
	if err := json.Unmarshal(b, &c); err != nil {
		return PolicyVersion{}, err
	}
	rules := make(map[domain.Currency]CurrencyRule, len(c.CurrencyRules))
	for _, r := range c.CurrencyRules {
		rules[domain.Currency(r.Currency)] = CurrencyRule{
			MaxPerTransactionMinor: r.MaxPerTransactionMinor, Max24hMinor: r.Max24hMinor,
			Max7dMinor: r.Max7dMinor, Max30dMinor: r.Max30dMinor,
			MaxPerHourCount: r.MaxPerHourCount, ApprovalThresholdMinor: r.ApprovalThresholdMinor,
		}
	}
	return PolicyVersion{
		OrgID: domain.OrgID(c.OrgID), Version: c.Version, CurrencyRules: rules,
		PayeeAllow: c.PayeeAllow, PayeeBlock: c.PayeeBlock, CategoryBlock: c.CategoryBlock,
	}, nil
}

// DefaultPolicyVersion is what an organisation that has never edited
// its policy is evaluated against: every limit unlimited, version 0
// so a real edit (version 1 onward) always reads back as distinct
// from "nothing has been set yet".
func DefaultPolicyVersion(org domain.OrgID) PolicyVersion {
	v, _ := NewPolicyVersion(org, 0, nil, nil, nil, nil)
	return v
}
