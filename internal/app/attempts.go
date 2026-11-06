package app

import (
	"context"
	"time"

	"enquoir/internal/domain"
)

// AttemptKind distinguishes what an attempt was.
type AttemptKind string

const (
	// KindAgentSubmission is an agent calling POST /v1/intents.
	KindAgentSubmission AttemptKind = "agent_submission"
	// KindRailCall is Enquoir calling Airwallex.
	KindRailCall AttemptKind = "rail_call"
)

// Operations: what was attempted.
const (
	OpSubmitIntent      = "submit_intent"
	OpCreateTransfer    = "create_transfer"
	OpGetTransfer       = "get_transfer"
	OpFindTransfer      = "find_transfer"
	OpCreateBeneficiary = "create_beneficiary"
	OpFindBeneficiary   = "find_beneficiary"
)

// AttemptOutcome is how an attempt ended.
type AttemptOutcome string

// Outcomes of an agent submission.
const (
	OutcomeStarted      AttemptOutcome = "started"       // a new payment began
	OutcomeAttached     AttemptOutcome = "attached"      // joined a run already starting
	OutcomeDeduplicated AttemptOutcome = "deduplicated"  // already had history; nothing started
	OutcomeInvalid      AttemptOutcome = "invalid"       // failed validation
	OutcomePayeeUnknown AttemptOutcome = "payee_unknown" // payee not the organisation's
	OutcomeError        AttemptOutcome = "error"         // Enquoir failed to process it
)

// Outcomes of a rail call.
const (
	OutcomeInProgress         AttemptOutcome = "in_progress"         // started, result not recorded (yet)
	OutcomeInterrupted        AttemptOutcome = "interrupted"         // started, never finished: the worker likely died
	OutcomeOK                 AttemptOutcome = "ok"                  // succeeded
	OutcomeNotFound           AttemptOutcome = "not_found"           // a lookup found nothing
	OutcomeRejected           AttemptOutcome = "rejected"            // the rail said a clear no
	OutcomeAmbiguous          AttemptOutcome = "ambiguous"           // outcome unclear: timeout, 5xx, network
	OutcomeDuplicateTransfers AttemptOutcome = "duplicate_transfers" // more than one transfer for one request ID
)

// InterruptedAfter is how long a rail call may stay unfinished before
// it reads as interrupted. Longer than any activity's timeout.
const InterruptedAfter = 2 * time.Minute

// Attempt is one agent submission or one Airwallex call.
type Attempt struct {
	ID        string
	OrgID     domain.OrgID
	Kind      AttemptKind
	Operation string
	Outcome   AttemptOutcome

	IntentKey      domain.IntentKey      // empty for an invalid submission or a beneficiary call
	BeneficiaryKey domain.BeneficiaryKey // for beneficiary calls
	AgentID        domain.AgentID        // for submissions
	AgentName      string                // filled in on reads

	// Rail calls.
	RequestID       string
	TransferID      string
	TransferStatus  string
	HTTPStatus      int
	ErrorCode       string
	ActivityAttempt int // Temporal's attempt number for the activity

	ErrorMessage string
	// Details carries what was submitted (payee, amount, currency,
	// reference, reason, reported state): plain strings, safe to show.
	Details map[string]string

	StartedAt  time.Time
	FinishedAt *time.Time
}

// Duration is how long the attempt took, if it finished.
func (a Attempt) Duration() *time.Duration {
	if a.FinishedAt == nil {
		return nil
	}
	d := a.FinishedAt.Sub(a.StartedAt)
	return &d
}

// AttemptFinish is how a started rail call ended.
type AttemptFinish struct {
	Outcome        AttemptOutcome
	TransferID     string
	TransferStatus string
	HTTPStatus     int
	ErrorCode      string
	ErrorMessage   string
	FinishedAt     time.Time
}

// AttemptFilter narrows a list of attempts. Zero fields don't filter.
type AttemptFilter struct {
	Kind      AttemptKind
	Operation string
	Outcomes  []AttemptOutcome
	AgentID   domain.AgentID
	IntentKey domain.IntentKey
	From, To  time.Time // StartedAt in [From, To)
	Limit     int
	Cursor    string
}

// AttemptPage is one page of attempts, newest first.
type AttemptPage struct {
	Attempts   []Attempt
	NextCursor string // empty on the last page
}

// AttemptLog records attempts. Recording is observability, never part
// of the money-safety guarantee; that is the event log's job.
type AttemptLog interface {
	// Record stores a finished attempt.
	Record(ctx context.Context, a Attempt) error
	// Start stores an attempt before it is made, as in_progress.
	Start(ctx context.Context, a Attempt) error
	// Finish records how a started attempt ended.
	Finish(ctx context.Context, org domain.OrgID, id string, f AttemptFinish) error
	// List returns an organisation's attempts, newest first. Rail calls
	// unfinished for longer than InterruptedAfter read as interrupted.
	List(ctx context.Context, org domain.OrgID, f AttemptFilter) (AttemptPage, error)
}
