package commerce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"enquoir/internal/domain"
)

// ExecutionStatus is where the payment a redemption permits stands. The
// outcome is the merchant's word: Enquoir checks it against what it
// authorized, but cannot verify it with the PSP.
type ExecutionStatus string

const (
	ExecutionPending   ExecutionStatus = "pending"
	ExecutionSucceeded ExecutionStatus = "succeeded"
	ExecutionFailed    ExecutionStatus = "failed"
	// ExecutionUnknown is not failed: the payment may have happened. Its
	// amount keeps counting until a reconciliation or webhook resolves it.
	ExecutionUnknown ExecutionStatus = "unknown"
)

func (s ExecutionStatus) finished() bool { return s == ExecutionSucceeded || s == ExecutionFailed }

// ParseExecutionStatus reads a status name, for filters.
func ParseExecutionStatus(s string) (ExecutionStatus, error) {
	switch st := ExecutionStatus(strings.ToLower(strings.TrimSpace(s))); st {
	case ExecutionPending, ExecutionSucceeded, ExecutionFailed, ExecutionUnknown:
		return st, nil
	}
	return "", fmt.Errorf("unknown status %q: must be pending, succeeded, failed or unknown", s)
}

// ExecutionReportDeadline is how long a redeemed execution may wait for
// its outcome. Past it, a still-pending execution is presumed unknown -
// its merchant may have crashed mid-charge - and must be reconciled.
const ExecutionReportDeadline = 30 * time.Minute

// Execution is the one payment a redeemed grant permits: what to charge
// and with which saved payment method, and what became of it. A grant
// has at most one.
type Execution struct {
	ID                       ExecutionID
	AuthorizationID          AuthorizationID
	CheckoutID               CheckoutID
	ExternalCheckoutID       ExternalID
	CheckoutHash             string
	MandateID                MandateID
	OrgID                    domain.OrgID
	MerchantID               MerchantID
	AmountMinor              int64
	Currency                 domain.Currency
	FundingMethod            FundingMethod
	Status                   ExecutionStatus
	ProviderPaymentReference string
	FailureReason            string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	ReportDeadline           time.Time
	FinishedAt               *time.Time
}

// NewExecution is the execution redeeming a at now permits, charging f.
func NewExecution(id ExecutionID, a Authorization, f FundingMethod, now time.Time) Execution {
	return Execution{
		ID: id, AuthorizationID: a.ID, CheckoutID: a.CheckoutID, ExternalCheckoutID: a.ExternalCheckoutID, CheckoutHash: a.CheckoutHash,
		MandateID: a.MandateID, OrgID: a.OrgID, MerchantID: a.MerchantID, AmountMinor: a.AmountMinor, Currency: a.Currency,
		FundingMethod: f, Status: ExecutionPending, CreatedAt: now, UpdatedAt: now, ReportDeadline: now.Add(ExecutionReportDeadline),
	}
}

// IdempotencyKey is what the merchant sends its PSP as the payment
// request's idempotency key: retrying the charge with it can't charge
// twice. Derived from the execution, so it is the same on every retry.
func (e Execution) IdempotencyKey() string { return "enquoir-" + string(e.ID) }

// ReportSource is how an outcome reached Enquoir.
type ReportSource string

const (
	// ReportDirect: the merchant's own call after charging.
	ReportDirect ReportSource = "direct"
	// ReportWebhook: a PSP event the merchant verified and forwarded,
	// with the PSP's event ID.
	ReportWebhook ReportSource = "webhook"
	// ReportReconciliation: the merchant looked the payment up at its PSP.
	ReportReconciliation ReportSource = "reconciliation"
	// ReportSweep: Enquoir, when the report deadline passed unreported.
	ReportSweep ReportSource = "sweep"
)

// ReportResult is what a report did.
type ReportResult string

const (
	ReportApplied   ReportResult = "applied"
	ReportDuplicate ReportResult = "duplicate" // the same as what is recorded: nothing changed
	ReportStale     ReportResult = "ignored"   // older than what is recorded: nothing changed
)

var (
	// ErrReportMismatch: the report's amount, currency, provider or payment
	// reference is not the execution's.
	ErrReportMismatch = errors.New("the report does not match the execution: amount, currency, provider and payment reference must be its own")
	// ErrOutcomeConflict: the report contradicts a finished payment.
	ErrOutcomeConflict = errors.New("the report contradicts the payment's recorded outcome; it was recorded for investigation")
	// ErrReconciliationRequired: an unknown outcome is resolved only by a
	// reconciliation or a verified webhook.
	ErrReconciliationRequired = errors.New("the outcome is unknown: look the payment up at your PSP and report it with POST .../reconcile")
	// ErrEventConflict: a webhook event ID already reported something else.
	ErrEventConflict = errors.New("this provider event ID was already reported with different content")
)

const maxFailureReason = 200

// ExecutionReport is an outcome the merchant reports.
type ExecutionReport struct {
	Source                   ReportSource
	Outcome                  ExecutionStatus
	Provider                 Provider
	ProviderPaymentReference string
	EventID                  string
	AmountMinor              int64
	Currency                 domain.Currency
	FailureReason            string
}

func reportErr(field string, err error) error { return &FieldError{Field: field, Err: err} }

// NewExecutionReport validates and normalises a report. No card data
// gets in: references and the failure reason may not hold one.
func NewExecutionReport(r ExecutionReport) (ExecutionReport, error) {
	switch r.Outcome {
	case ExecutionSucceeded, ExecutionFailed, ExecutionUnknown:
	default:
		return ExecutionReport{}, reportErr("outcome", errors.New("outcome must be succeeded, failed or unknown"))
	}
	p, err := NewProvider(string(r.Provider))
	if err != nil {
		return ExecutionReport{}, reportErr("provider", err)
	}
	r.Provider = p
	r.Currency = domain.Currency(strings.ToUpper(strings.TrimSpace(string(r.Currency))))
	if !r.Currency.Supported() {
		return ExecutionReport{}, reportErr("currency", errors.New("currency must be USD, EUR or GBP"))
	}
	if r.AmountMinor <= 0 {
		return ExecutionReport{}, reportErr("amount_minor", errors.New("amount_minor must be positive"))
	}
	if strings.TrimSpace(r.ProviderPaymentReference) != "" {
		ref, err := NewProviderReference(r.ProviderPaymentReference)
		if err != nil {
			return ExecutionReport{}, reportErr("provider_payment_reference", err)
		}
		r.ProviderPaymentReference = string(ref)
	} else {
		r.ProviderPaymentReference = ""
	}
	if r.Outcome == ExecutionSucceeded && r.ProviderPaymentReference == "" {
		return ExecutionReport{}, reportErr("provider_payment_reference", errors.New("a succeeded payment needs the PSP's payment reference"))
	}
	r.EventID = strings.TrimSpace(r.EventID)
	if r.Source == ReportWebhook {
		ext, err := NewExternalID(r.EventID)
		if err != nil {
			return ExecutionReport{}, reportErr("provider_event_id", err)
		}
		r.EventID = string(ext)
	}
	r.FailureReason = strings.TrimSpace(r.FailureReason)
	if utf8.RuneCountInString(r.FailureReason) > maxFailureReason {
		return ExecutionReport{}, reportErr("failure_reason", fmt.Errorf("failure_reason must be at most %d characters", maxFailureReason))
	}
	if HoldsCardNumber(r.FailureReason) {
		return ExecutionReport{}, reportErr("failure_reason", fmt.Errorf("failure_reason %w", ErrLooksLikeCardNumber))
	}
	return r, nil
}

// Fingerprint identifies a report's content, to tell a repeated webhook
// event from the same event ID reporting something else.
func (r ExecutionReport) Fingerprint() string {
	b, _ := json.Marshal([]any{r.Outcome, r.Provider, r.ProviderPaymentReference, r.AmountMinor, r.Currency, r.FailureReason})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Apply is e after report r at now, and what the report did:
//
//   - The report must match the execution: amount, currency, provider,
//     and the payment reference once one is known.
//   - pending: any outcome applies.
//   - unknown: unknown again is a duplicate; a definitive outcome applies
//     only from a reconciliation or a webhook.
//   - succeeded or failed: the same again is a duplicate; unknown is
//     stale and ignored; the other outcome contradicts it.
func (e Execution) Apply(r ExecutionReport, now time.Time) (Execution, ReportResult, error) {
	if r.AmountMinor != e.AmountMinor || r.Currency != e.Currency || r.Provider != e.FundingMethod.Provider {
		return e, "", ErrReportMismatch
	}
	if e.ProviderPaymentReference != "" && r.ProviderPaymentReference != "" && r.ProviderPaymentReference != e.ProviderPaymentReference {
		return e, "", ErrReportMismatch
	}
	switch {
	case e.Status.finished() && r.Outcome == e.Status:
		return e, ReportDuplicate, nil
	case e.Status.finished() && r.Outcome == ExecutionUnknown:
		return e, ReportStale, nil
	case e.Status.finished():
		return e, "", ErrOutcomeConflict
	case e.Status == ExecutionUnknown && r.Outcome == ExecutionUnknown:
		return e, ReportDuplicate, nil
	case e.Status == ExecutionUnknown && r.Source != ReportReconciliation && r.Source != ReportWebhook:
		return e, "", ErrReconciliationRequired
	}
	e.Status, e.UpdatedAt = r.Outcome, now
	if r.ProviderPaymentReference != "" {
		e.ProviderPaymentReference = r.ProviderPaymentReference
	}
	if r.FailureReason != "" {
		e.FailureReason = r.FailureReason
	}
	if e.Status.finished() {
		e.FinishedAt = &now
	}
	return e, ReportApplied, nil
}

// ExecutionEventKind is what an entry in an execution's history records.
type ExecutionEventKind string

const (
	EventRedeemed  ExecutionEventKind = "redeemed"
	EventReported  ExecutionEventKind = "reported"  // applied
	EventDuplicate ExecutionEventKind = "duplicate" // repeated, nothing changed
	EventIgnored   ExecutionEventKind = "ignored"   // stale, nothing changed
	EventRefused   ExecutionEventKind = "refused"   // mismatched or contradictory: for investigation
	EventSwept     ExecutionEventKind = "swept"     // the deadline passed: presumed unknown
)

// ExecutionEvent is one entry in an execution's append-only history.
type ExecutionEvent struct {
	Sequence    int
	Kind        ExecutionEventKind
	Source      ReportSource
	Outcome     ExecutionStatus
	StatusAfter ExecutionStatus
	Reference   string
	EventID     string
	AmountMinor int64
	Currency    domain.Currency
	Detail      string
	At          time.Time
}

// EventFor is the history entry recording report r on an execution that
// ended up in status after it, with result, or refused with err.
func EventFor(r ExecutionReport, after ExecutionStatus, result ReportResult, err error, at time.Time) ExecutionEvent {
	ev := ExecutionEvent{Source: r.Source, Outcome: r.Outcome, StatusAfter: after, Reference: r.ProviderPaymentReference,
		EventID: r.EventID, AmountMinor: r.AmountMinor, Currency: r.Currency, Detail: r.FailureReason, At: at}
	switch {
	case err != nil:
		ev.Kind, ev.Detail = EventRefused, err.Error()
	case result == ReportDuplicate:
		ev.Kind = EventDuplicate
	case result == ReportStale:
		ev.Kind = EventIgnored
	default:
		ev.Kind = EventReported
	}
	return ev
}
