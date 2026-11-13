package app

import (
	"context"
	"fmt"
	"time"

	"enquoir/internal/domain"
)

// StateReceived is a payment the API accepted that no worker has
// recorded anything for yet. It is a read-model state only, never a
// domain.State: nothing has happened to the payment.
const StateReceived = "RECEIVED"

// Payment is the dashboard's view of one intent.
type Payment struct {
	Key            domain.IntentKey
	OrgID          domain.OrgID
	State          string // a domain.State, or StateReceived
	Payee          string
	PayeeName      string // empty if not registered through Enquoir
	Amount         domain.Money
	Reference      string
	Reason         string
	AgentID        domain.AgentID // empty for tooling-submitted intents
	AgentName      string
	TransferID     string
	FailureReason  string
	CreatedAt      time.Time
	StateChangedAt *time.Time
	Submissions    int // agent submissions of this intent
	RailCalls      int // Airwallex calls made for it
}

// Terminal reports whether nothing more will happen to the payment.
func (p Payment) Terminal() bool {
	return p.State != StateReceived && domain.State(p.State).IsTerminal()
}

// PaymentFilter narrows a list of payments. Zero fields don't filter.
type PaymentFilter struct {
	States    []string
	AgentID   domain.AgentID
	Payee     string
	Currency  domain.Currency
	Reference string
	From, To  time.Time // CreatedAt in [From, To)
	Limit     int
	Cursor    string
}

// PaymentPage is one page of payments, newest first.
type PaymentPage struct {
	Payments   []Payment
	NextCursor string
}

// PaymentReader reads the payment read model, scoped by organisation.
type PaymentReader interface {
	ListPayments(ctx context.Context, org domain.OrgID, f PaymentFilter) (PaymentPage, error)
	// GetPayment returns ErrNotFound for a key not in org.
	GetPayment(ctx context.Context, org domain.OrgID, key domain.IntentKey) (Payment, error)
}

const (
	defaultPageSize  = 50
	maxPageSize      = 200
	maxAttemptsShown = 500
)

func pageSize(n int) (int, error) {
	switch {
	case n == 0:
		return defaultPageSize, nil
	case n < 1 || n > maxPageSize:
		return 0, &ValidationError{Field: "limit", Message: fmt.Sprintf("limit must be 1 to %d", maxPageSize)}
	}
	return n, nil
}

func period(from, to time.Time) error {
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return &ValidationError{Field: "from", Message: "from must be before to"}
	}
	return nil
}

var knownStates = map[string]bool{
	StateReceived: true, string(domain.PROPOSED): true, string(domain.AWAITING_APPROVAL): true, string(domain.AUTHORIZED): true, string(domain.IN_FLIGHT): true,
	string(domain.UNKNOWN): true, string(domain.APPLIED): true, string(domain.FAILED): true, string(domain.BLOCKED): true,
}

// ListPayments lists an organisation's payments, newest first.
type ListPayments struct{ Payments PaymentReader }

func (l *ListPayments) Run(ctx context.Context, org domain.OrgID, f PaymentFilter) (PaymentPage, error) {
	var err error
	if f.Limit, err = pageSize(f.Limit); err != nil {
		return PaymentPage{}, err
	}
	if err := period(f.From, f.To); err != nil {
		return PaymentPage{}, err
	}
	for _, s := range f.States {
		if !knownStates[s] {
			return PaymentPage{}, &ValidationError{Field: "state", Message: fmt.Sprintf("unknown state %q", s)}
		}
	}
	if f.Currency != "" && !f.Currency.Supported() {
		return PaymentPage{}, &ValidationError{Field: "currency", Message: fmt.Sprintf("unsupported currency %q", f.Currency)}
	}
	return l.Payments.ListPayments(ctx, org, f)
}

// PaymentDetail is one payment with everything behind it: its
// hash-chained history, whether that history verifies, and every
// attempt made for it, oldest first.
type PaymentDetail struct {
	Payment    Payment
	Events     domain.Chain
	ChainError error // nil when the chain verifies
	Attempts   []Attempt
}

// GetPayment fetches one payment in full.
type GetPayment struct {
	Payments PaymentReader
	Events   EventLog
	Attempts AttemptLog
}

func (g *GetPayment) Run(ctx context.Context, org domain.OrgID, key domain.IntentKey) (PaymentDetail, error) {
	p, err := g.Payments.GetPayment(ctx, org, key)
	if err != nil {
		return PaymentDetail{}, err
	}
	chain, err := g.Events.Load(ctx, org, key)
	if err != nil {
		return PaymentDetail{}, err
	}
	page, err := g.Attempts.List(ctx, org, AttemptFilter{IntentKey: key, Limit: maxAttemptsShown})
	if err != nil {
		return PaymentDetail{}, err
	}
	attempts := make([]Attempt, len(page.Attempts))
	for i, a := range page.Attempts {
		attempts[len(attempts)-1-i] = a
	}
	return PaymentDetail{Payment: p, Events: chain, ChainError: chain.Verify(), Attempts: attempts}, nil
}

var (
	knownKinds      = map[AttemptKind]bool{KindAgentSubmission: true, KindRailCall: true}
	knownOperations = map[string]bool{OpSubmitIntent: true, OpCreateTransfer: true, OpGetTransfer: true, OpFindTransfer: true, OpCreateBeneficiary: true, OpFindBeneficiary: true}
	knownOutcomes   = map[AttemptOutcome]bool{
		OutcomeStarted: true, OutcomeAttached: true, OutcomeDeduplicated: true, OutcomeInvalid: true, OutcomePayeeUnknown: true, OutcomeError: true,
		OutcomeInProgress: true, OutcomeInterrupted: true, OutcomeOK: true, OutcomeNotFound: true, OutcomeRejected: true, OutcomeAmbiguous: true, OutcomeDuplicateTransfers: true,
		OutcomePolicyDenied: true,
	}
)

// ListAttempts lists an organisation's attempts, newest first.
type ListAttempts struct{ Attempts AttemptLog }

func (l *ListAttempts) Run(ctx context.Context, org domain.OrgID, f AttemptFilter) (AttemptPage, error) {
	var err error
	if f.Limit, err = pageSize(f.Limit); err != nil {
		return AttemptPage{}, err
	}
	if err := period(f.From, f.To); err != nil {
		return AttemptPage{}, err
	}
	if f.Kind != "" && !knownKinds[f.Kind] {
		return AttemptPage{}, &ValidationError{Field: "kind", Message: fmt.Sprintf("unknown kind %q", f.Kind)}
	}
	if f.Operation != "" && !knownOperations[f.Operation] {
		return AttemptPage{}, &ValidationError{Field: "operation", Message: fmt.Sprintf("unknown operation %q", f.Operation)}
	}
	for _, o := range f.Outcomes {
		if !knownOutcomes[o] {
			return AttemptPage{}, &ValidationError{Field: "outcome", Message: fmt.Sprintf("unknown outcome %q", o)}
		}
	}
	return l.Attempts.List(ctx, org, f)
}

// ListBeneficiaries lists the payees an organisation registered.
type ListBeneficiaries struct{ Beneficiaries BeneficiaryStore }

func (l *ListBeneficiaries) Run(ctx context.Context, org domain.OrgID) ([]RegisteredBeneficiary, error) {
	return l.Beneficiaries.List(ctx, org)
}
