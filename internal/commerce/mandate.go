package commerce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"enquoir/internal/domain"
)

// MandateRequestID identifies a request for a customer's consent. A
// request is not a mandate: it grants nothing.
type MandateRequestID string

// MandateID identifies a mandate: a customer's bounded grant of
// spending authority to one of a merchant's agents.
type MandateID string

// ConsentRecordID identifies the immutable evidence of a customer's
// decision on a mandate request.
type ConsentRecordID string

// ApprovalMode is how purchases under a mandate are approved:
// autonomously within its scope, or by the customer each time.
type ApprovalMode string

const (
	Autonomous     ApprovalMode = "autonomous"
	PerTransaction ApprovalMode = "per_transaction"
)

func ParseApprovalMode(s string) (ApprovalMode, error) {
	switch m := ApprovalMode(strings.ToLower(strings.TrimSpace(s))); m {
	case Autonomous, PerTransaction:
		return m, nil
	}
	return "", fmt.Errorf("unknown approval mode %q: must be autonomous or per_transaction", s)
}

// MaxMandateDuration is the longest a mandate may last.
const MaxMandateDuration = 90 * 24 * time.Hour

const (
	maxCategories   = 50
	maxTransactions = 100000
)

var categoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,49}$`)

// MandateScope bounds what a mandate allows: one currency, a maximum per
// purchase and in total (integer minor units), optionally a maximum
// number of purchases (0: no count limit) and allowed product
// categories (empty: any).
type MandateScope struct {
	Currency               domain.Currency
	MaxPerTransactionMinor int64
	MaxTotalMinor          int64
	MaxTransactions        int
	AllowedCategories      []string
}

func scopeErr(field string, err error) error {
	return &FieldError{Field: "scope." + field, Err: err}
}

// NewMandateScope validates a scope; categories are trimmed,
// lower-cased, de-duplicated and sorted.
func NewMandateScope(currency domain.Currency, perTx, total int64, count int, categories []string) (MandateScope, error) {
	currency = domain.Currency(strings.ToUpper(strings.TrimSpace(string(currency))))
	if !currency.Supported() {
		return MandateScope{}, scopeErr("currency", fmt.Errorf("currency %q is not supported: use USD, EUR or GBP", currency))
	}
	if _, err := domain.NewMoney(perTx, currency); err != nil {
		return MandateScope{}, scopeErr("max_transaction_minor", errors.New("max_transaction_minor must be a positive whole number of minor units"))
	}
	if _, err := domain.NewMoney(total, currency); err != nil {
		return MandateScope{}, scopeErr("max_cumulative_minor", errors.New("max_cumulative_minor must be a positive whole number of minor units"))
	}
	if total < perTx {
		return MandateScope{}, scopeErr("max_cumulative_minor", errors.New("max_cumulative_minor must be at least max_transaction_minor"))
	}
	if count < 0 || count > maxTransactions {
		return MandateScope{}, scopeErr("max_transactions", fmt.Errorf("max_transactions must be 0 (no limit) to %d", maxTransactions))
	}
	if len(categories) > maxCategories {
		return MandateScope{}, scopeErr("allowed_categories", fmt.Errorf("at most %d allowed categories", maxCategories))
	}
	seen := map[string]bool{}
	out := []string{}
	for _, c := range categories {
		c = strings.ToLower(strings.TrimSpace(c))
		if !categoryPattern.MatchString(c) {
			return MandateScope{}, scopeErr("allowed_categories", errors.New("each category must be 1 to 50 lower-case letters, digits, '_' or '-'"))
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return MandateScope{Currency: currency, MaxPerTransactionMinor: perTx, MaxTotalMinor: total, MaxTransactions: count, AllowedCategories: out}, nil
}

// MandateParties are who a mandate concerns, with the names and card
// display the customer is shown: they are part of what is consented to.
type MandateParties struct {
	OrgID           domain.OrgID
	MerchantID      MerchantID
	MerchantName    string
	CustomerID      CustomerID
	AgentID         AgentID
	AgentName       string
	FundingMethodID FundingMethodID
	FundingDisplay  FundingDisplay
}

// MandateTerms are exactly what a customer is asked to approve. They
// are frozen when the request is made; Hash binds every one of them.
type MandateTerms struct {
	Parties      MandateParties
	ApprovalMode ApprovalMode
	Scope        MandateScope
	ExpiresAt    time.Time
}

// NewMandateTerms checks the expiry - after now, at most 90 days ahead,
// kept to the second in UTC - and freezes the terms.
func NewMandateTerms(p MandateParties, mode ApprovalMode, scope MandateScope, expiresAt, now time.Time) (MandateTerms, error) {
	expiresAt = expiresAt.UTC().Truncate(time.Second)
	if !expiresAt.After(now) {
		return MandateTerms{}, &FieldError{Field: "expires_at", Err: errors.New("expires_at must be in the future")}
	}
	if expiresAt.After(now.Add(MaxMandateDuration)) {
		return MandateTerms{}, &FieldError{Field: "expires_at", Err: errors.New("expires_at must be at most 90 days ahead")}
	}
	return MandateTerms{Parties: p, ApprovalMode: mode, Scope: scope, ExpiresAt: expiresAt}, nil
}

const termsVersion = "mandate-terms/v1"

// canonicalTerms is the snapshot's wire form. Field order is fixed by
// the struct, categories are sorted and never null, the time is RFC
// 3339 UTC to the second: the same terms always give the same bytes.
type canonicalTerms struct {
	Version                string   `json:"version"`
	OrgID                  string   `json:"organisation_id"`
	MerchantID             string   `json:"merchant_id"`
	MerchantName           string   `json:"merchant_name"`
	CustomerID             string   `json:"customer_id"`
	AgentID                string   `json:"agent_id"`
	AgentName              string   `json:"agent_name"`
	FundingMethodID        string   `json:"funding_method_id"`
	FundingBrand           string   `json:"funding_brand"`
	FundingLastFour        string   `json:"funding_last_four"`
	ApprovalMode           string   `json:"approval_mode"`
	Currency               string   `json:"currency"`
	MaxPerTransactionMinor int64    `json:"max_transaction_minor"`
	MaxTotalMinor          int64    `json:"max_cumulative_minor"`
	MaxTransactions        int      `json:"max_transactions"`
	AllowedCategories      []string `json:"allowed_categories"`
	ExpiresAt              string   `json:"expires_at"`
}

// Canonical is the terms' snapshot: stored with the request, the
// mandate and the consent record, and what Hash is computed over.
func (t MandateTerms) Canonical() []byte {
	cats := append([]string{}, t.Scope.AllowedCategories...)
	sort.Strings(cats)
	p := t.Parties
	b, err := json.Marshal(canonicalTerms{
		Version: termsVersion, OrgID: string(p.OrgID), MerchantID: string(p.MerchantID), MerchantName: p.MerchantName,
		CustomerID: string(p.CustomerID), AgentID: string(p.AgentID), AgentName: p.AgentName,
		FundingMethodID: string(p.FundingMethodID), FundingBrand: p.FundingDisplay.Brand, FundingLastFour: p.FundingDisplay.LastFour,
		ApprovalMode: string(t.ApprovalMode), Currency: string(t.Scope.Currency),
		MaxPerTransactionMinor: t.Scope.MaxPerTransactionMinor, MaxTotalMinor: t.Scope.MaxTotalMinor,
		MaxTransactions: t.Scope.MaxTransactions, AllowedCategories: cats,
		ExpiresAt: t.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		panic("commerce: marshalling mandate terms: " + err.Error()) // only strings and numbers: cannot fail
	}
	return b
}

// Hash is "sha256:" and the hex SHA-256 of the canonical snapshot.
func (t MandateTerms) Hash() string {
	sum := sha256.Sum256(t.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseMandateTerms reads a stored snapshot back.
func ParseMandateTerms(b []byte) (MandateTerms, error) {
	var c canonicalTerms
	if err := json.Unmarshal(b, &c); err != nil {
		return MandateTerms{}, fmt.Errorf("reading mandate terms: %w", err)
	}
	if c.Version != termsVersion {
		return MandateTerms{}, fmt.Errorf("unknown mandate terms version %q", c.Version)
	}
	exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return MandateTerms{}, fmt.Errorf("reading mandate terms: %w", err)
	}
	if c.AllowedCategories == nil {
		c.AllowedCategories = []string{}
	}
	return MandateTerms{
		Parties: MandateParties{
			OrgID: domain.OrgID(c.OrgID), MerchantID: MerchantID(c.MerchantID), MerchantName: c.MerchantName,
			CustomerID: CustomerID(c.CustomerID), AgentID: AgentID(c.AgentID), AgentName: c.AgentName,
			FundingMethodID: FundingMethodID(c.FundingMethodID), FundingDisplay: FundingDisplay{Brand: c.FundingBrand, LastFour: c.FundingLastFour},
		},
		ApprovalMode: ApprovalMode(c.ApprovalMode),
		Scope: MandateScope{
			Currency: domain.Currency(c.Currency), MaxPerTransactionMinor: c.MaxPerTransactionMinor, MaxTotalMinor: c.MaxTotalMinor,
			MaxTransactions: c.MaxTransactions, AllowedCategories: c.AllowedCategories,
		},
		ExpiresAt: exp.UTC(),
	}, nil
}

// RequestStatus is where a mandate request stands.
type RequestStatus string

const (
	RequestPending  RequestStatus = "pending"
	RequestApproved RequestStatus = "approved"
	RequestRejected RequestStatus = "rejected"
	RequestExpired  RequestStatus = "expired"
)

// ConsentLinkTTL is how long a consent link, and the request it is for,
// stays open.
const ConsentLinkTTL = time.Hour

// MandateRequest asks a customer to approve terms. Only the customer's
// approval, through Enquoir's consent flow, turns it into a mandate.
type MandateRequest struct {
	ID         MandateRequestID
	ExternalID ExternalID // the merchant's reference for the request
	Terms      MandateTerms
	Status     RequestStatus // as stored: pending, approved or rejected
	CreatedAt  time.Time
	ExpiresAt  time.Time
	DecidedAt  *time.Time
	MandateID  MandateID // once approved
}

// StatusAt is the request's status at now: a pending request past its
// expiry is expired.
func (r MandateRequest) StatusAt(now time.Time) RequestStatus {
	if r.Status == RequestPending && !now.Before(r.ExpiresAt) {
		return RequestExpired
	}
	return r.Status
}

// MandateStatus is a mandate's effective status.
type MandateStatus string

const (
	MandateActive    MandateStatus = "active"
	MandateSuspended MandateStatus = "suspended"
	MandateExpired   MandateStatus = "expired"
	MandateRevoked   MandateStatus = "revoked"
)

// ParseMandateStatus reads a status name, for filters.
func ParseMandateStatus(s string) (MandateStatus, error) {
	switch st := MandateStatus(strings.ToLower(strings.TrimSpace(s))); st {
	case MandateActive, MandateSuspended, MandateExpired, MandateRevoked:
		return st, nil
	}
	return "", fmt.Errorf("unknown status %q: must be active, suspended, expired or revoked", s)
}

// Dependencies are whether each thing a mandate names may still take
// part in commerce.
type Dependencies struct {
	IntegrationActive, CustomerActive, AgentActive, FundingMethodActive bool
}

func (d Dependencies) all() bool {
	return d.IntegrationActive && d.CustomerActive && d.AgentActive && d.FundingMethodActive
}

// Mandate is a customer's approved grant of spending authority, on the
// exact terms they approved.
type Mandate struct {
	ID              MandateID
	RequestID       MandateRequestID
	ConsentRecordID ConsentRecordID
	Terms           MandateTerms
	ApprovedAt      time.Time
	Revoked         bool
	RevokedAt       *time.Time
}

// StatusAt is the mandate's status at now: revoked, else expired, else
// suspended while anything it names is disabled, else active.
func (m Mandate) StatusAt(now time.Time, d Dependencies) MandateStatus {
	switch {
	case m.Revoked:
		return MandateRevoked
	case !now.Before(m.Terms.ExpiresAt):
		return MandateExpired
	case !d.all():
		return MandateSuspended
	}
	return MandateActive
}

// ConsentDecision is what the customer decided.
type ConsentDecision string

const (
	ConsentApproved ConsentDecision = "approved"
	ConsentRejected ConsentDecision = "rejected"
)

// ConsentMethodEmailLink is consent given through the one-time link
// Enquoir emailed to the customer's registered address.
const ConsentMethodEmailLink = "email_link"

// ConsentRecord is immutable evidence of a customer's decision: the
// exact snapshot they were shown, its hash, how and when they decided,
// and from where.
type ConsentRecord struct {
	ID         ConsentRecordID
	RequestID  MandateRequestID
	OrgID      domain.OrgID
	MerchantID MerchantID
	CustomerID CustomerID
	Decision   ConsentDecision
	Snapshot   []byte
	TermsHash  string
	Method     string
	IP         string
	UserAgent  string
	At         time.Time
}
