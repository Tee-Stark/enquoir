package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"enquoir/internal/domain"
)

// ErrIntentNotFound means no event has ever been recorded for a key -
// as far as this system has observed, that intent does not exist.
var ErrIntentNotFound = errors.New("intent not found")

// ErrPayeeUnknown means an intent names a payee its organisation never
// registered - possibly another organisation's, which the shared rail
// account would otherwise pay.
var ErrPayeeUnknown = errors.New("payee not registered by this organisation")

// PaymentStarter starts, or attaches to, the durable workflow that
// carries one Intent to a terminal state. Submitting the same intent
// twice must attach to the run already in progress, never start a
// second one.
type PaymentStarter interface {
	Start(ctx context.Context, intent domain.Intent) error
}

// SubmitIntentCommand is an agent's submission, as sent.
type SubmitIntentCommand struct {
	OrgID     domain.OrgID
	Agent     domain.AgentID
	Payee     string
	Amount    string // major units, e.g. "10.50"
	Currency  string
	Reference string
	Reason    string
}

// SubmitIntentResult is what submitting an intent resolves to: its
// key, its state as currently recorded - PROPOSED for a brand new
// submission, or whatever a run already in progress has reached - and
// what the submission did.
type SubmitIntentResult struct {
	Key     domain.IntentKey
	State   domain.State
	Outcome AttemptOutcome
}

// SubmitIntent is the submit-intent use case. Every submission, valid
// or not, is recorded as an agent_submission attempt.
type SubmitIntent struct {
	Starter  PaymentStarter
	Events   EventLog
	Payees   BeneficiaryStore
	Intents  IntentRepository
	Attempts AttemptLog
	IDs      IDGenerator
	Clock    Clock
}

// Run validates the submission and starts intent's workflow only if
// nothing has ever been recorded for it. The event log, not the
// workflow engine, decides: the engine only dedupes a run while it is
// still running and forgets finished runs after its retention period,
// while a recorded history is permanent. A new run always begins at
// PROPOSED, which no recorded state may legally move back to, so an
// intent with any history is reported as it stands - a run in progress
// carries on by itself, and moving a FAILED or UNKNOWN intent on is a
// reconciler's job, never a resubmission's.
func (s *SubmitIntent) Run(ctx context.Context, cmd SubmitIntentCommand) (SubmitIntentResult, error) {
	attempt := Attempt{
		ID:        s.IDs.NewID("att"),
		OrgID:     cmd.OrgID,
		Kind:      KindAgentSubmission,
		Operation: OpSubmitIntent,
		AgentID:   cmd.Agent,
		StartedAt: s.Clock.Now(),
		Details: map[string]string{
			"payee": cmd.Payee, "amount": cmd.Amount, "currency": cmd.Currency,
			"reference": cmd.Reference, "reason": cmd.Reason,
		},
	}
	result, err := s.submit(ctx, cmd, &attempt)
	if err != nil {
		attempt.ErrorMessage = err.Error()
		switch {
		case isValidation(err):
			attempt.Outcome = OutcomeInvalid
		case err == ErrPayeeUnknown:
			attempt.Outcome = OutcomePayeeUnknown
		default:
			attempt.Outcome = OutcomeError
			attempt.ErrorMessage = "Enquoir could not process the submission"
		}
	} else {
		attempt.Outcome = result.Outcome
		attempt.Details["reported_state"] = string(result.State)
	}
	finished := s.Clock.Now()
	attempt.FinishedAt = &finished
	if rerr := s.Attempts.Record(ctx, attempt); rerr != nil && err == nil {
		return SubmitIntentResult{}, rerr
	}
	return result, err
}

func (s *SubmitIntent) submit(ctx context.Context, cmd SubmitIntentCommand, attempt *Attempt) (SubmitIntentResult, error) {
	intent, err := buildIntent(cmd)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	attempt.IntentKey = intent.Key

	owned, err := s.Payees.Owns(ctx, intent.OrgID, intent.Payee)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	if !owned {
		return SubmitIntentResult{}, ErrPayeeUnknown
	}
	_, created, err := s.Intents.Save(ctx, intent)
	if err != nil {
		return SubmitIntentResult{}, err
	}

	chain, err := s.Events.Load(ctx, intent.OrgID, intent.Key)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	if len(chain) > 0 {
		return SubmitIntentResult{Key: intent.Key, State: domain.State(chain[len(chain)-1].Type), Outcome: OutcomeDeduplicated}, nil
	}

	// No history yet: either brand new, or a run started moments ago
	// that hasn't recorded anything - starting attaches to that run.
	if err := s.Starter.Start(ctx, intent); err != nil {
		return SubmitIntentResult{}, err
	}
	outcome := OutcomeStarted
	if !created {
		outcome = OutcomeAttached
	}
	return SubmitIntentResult{Key: intent.Key, State: domain.PROPOSED, Outcome: outcome}, nil
}

// buildIntent validates a submission into an Intent, naming the field
// at fault.
func buildIntent(cmd SubmitIntentCommand) (domain.Intent, error) {
	if strings.TrimSpace(cmd.Payee) == "" {
		return domain.Intent{}, &ValidationError{Field: "payee", Message: "payee is required"}
	}
	currency := domain.Currency(strings.ToUpper(strings.TrimSpace(cmd.Currency)))
	if !currency.Supported() {
		return domain.Intent{}, &ValidationError{Field: "currency", Message: fmt.Sprintf("unsupported currency %q: must be USD, EUR or GBP", cmd.Currency)}
	}
	amount, err := domain.ParseMajorUnits(cmd.Amount, currency)
	if err != nil {
		return domain.Intent{}, invalid("amount", err)
	}
	if strings.TrimSpace(cmd.Reference) == "" {
		return domain.Intent{}, &ValidationError{Field: "reference", Message: "reference is required"}
	}
	intent, err := domain.NewIntent(cmd.OrgID, cmd.Payee, amount.MinorUnits(), string(currency), cmd.Reference, cmd.Reason)
	if err != nil {
		return domain.Intent{}, invalid("intent", err)
	}
	intent.Agent = cmd.Agent
	return intent, nil
}

func isValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

// GetIntentResult is one intent's recorded state and full event
// history, oldest first.
type GetIntentResult struct {
	Key    domain.IntentKey
	State  domain.State
	Events domain.Chain
}

// GetIntent is the get-intent-by-key use case.
type GetIntent struct {
	Events EventLog
}

func (g *GetIntent) Run(ctx context.Context, org domain.OrgID, key domain.IntentKey) (GetIntentResult, error) {
	chain, err := g.Events.Load(ctx, org, key)
	if err != nil {
		return GetIntentResult{}, err
	}
	if len(chain) == 0 {
		return GetIntentResult{}, ErrIntentNotFound
	}

	return GetIntentResult{
		Key:    key,
		State:  domain.State(chain[len(chain)-1].Type),
		Events: chain,
	}, nil
}
