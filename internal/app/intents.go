package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

// ErrIntentNotFound means no event has ever been recorded for a key -
// as far as this system has observed, that intent does not exist.
var ErrIntentNotFound = errors.New("intent not found")

// ErrPayeeUnknown means an intent names a payee its organisation never
// registered - possibly another organisation's, which the shared rail
// account would otherwise pay.
var ErrPayeeUnknown = errors.New("payee not registered by this organisation")

// PolicyDeniedError means the organisation's spending policy denied a
// candidate payment, or (until escalation exists, Phase 6d) decided
// to escalate it - until then, escalate is treated identically to
// deny, distinguishable only by Decision.Outcome.
type PolicyDeniedError struct {
	Decision policy.Decision
}

func (e *PolicyDeniedError) Error() string {
	details := make([]string, 0, len(e.Decision.Reasons))
	for _, r := range e.Decision.Reasons {
		details = append(details, r.Detail)
	}
	return fmt.Sprintf("denied by the organisation's spending policy (version %d): %s", e.Decision.Version, strings.Join(details, "; "))
}

// PaymentStarter starts, or attaches to, the durable workflow that
// carries one Intent to a terminal state. Submitting the same intent
// twice must attach to the run already in progress, never start a
// second one. escalate means the organisation's policy decided, at
// submission time, that this payment must wait for an owner or
// admin's approval before it can be authorized.
type PaymentStarter interface {
	Start(ctx context.Context, intent domain.Intent, escalate bool) error
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
	Treasuries TreasuryStore
	Starter    PaymentStarter
	Events     EventLog
	Payees     BeneficiaryStore
	Intents    IntentRepository
	Attempts   AttemptLog
	IDs        IDGenerator
	Clock      Clock
	// Policies evaluates a candidate payment against the organisation's
	// spending policy. Nil skips the check and always allows - existing
	// callers with no stake in policy evaluation need no change.
	Policies policy.Store
	Audit    *Audit
	// Members and Emails notify every owner/admin when a payment
	// escalates. Both nil-safe: left unset, no email is sent.
	Members MemberStore
	Emails  *Emails
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
		var policyDenied *PolicyDeniedError
		switch {
		case isValidation(err):
			attempt.Outcome = OutcomeInvalid
		case err == ErrPayeeUnknown:
			attempt.Outcome = OutcomePayeeUnknown
		case errors.As(err, &policyDenied):
			attempt.Outcome = OutcomePolicyDenied
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
	treasury, err := s.Treasuries.TreasuryOf(ctx, cmd.OrgID)
	if err != nil {
		return SubmitIntentResult{}, fmt.Errorf("finding the organisation's treasury: %w", err)
	}
	intent, err := buildIntent(cmd, treasury.ID)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	attempt.IntentKey = intent.Key

	// An intent recorded before treasuries has its key without one. A
	// resubmission of it is the same payment: report it as it stands,
	// never derive a new key and pay again.
	legacy, err := s.Events.Load(ctx, intent.OrgID, intent.LegacyKey())
	if err != nil {
		return SubmitIntentResult{}, err
	}
	if len(legacy) > 0 {
		attempt.IntentKey = intent.LegacyKey()
		return SubmitIntentResult{Key: intent.LegacyKey(), State: domain.State(legacy[len(legacy)-1].Type), Outcome: OutcomeDeduplicated}, nil
	}

	owned, err := s.Payees.Owns(ctx, intent.OrgID, intent.TreasuryID, intent.Payee)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	if !owned {
		return SubmitIntentResult{}, ErrPayeeUnknown
	}
	// The dedupe check on the real key comes before policy evaluation,
	// not after: an event can only exist for this key once some prior
	// call's Intents.Save already created the row, so this read is
	// safe before Save runs here too. Checking first means a retry of
	// an intent that already has a chain short-circuits straight to
	// OutcomeDeduplicated, instead of re-running policy's real side
	// effects (a denial, an audit record, escalation emails) on a
	// payment that already went through.
	chain, err := s.Events.Load(ctx, intent.OrgID, intent.Key)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	if len(chain) > 0 {
		return SubmitIntentResult{Key: intent.Key, State: domain.State(chain[len(chain)-1].Type), Outcome: OutcomeDeduplicated}, nil
	}

	var escalate bool
	if s.Policies != nil {
		payee := intent.Payee
		decision, err := s.Policies.EvaluateAndReserve(ctx, intent.OrgID, policy.EvaluationInput{
			Context: policy.ContextTreasury, Currency: intent.Amount.Currency(),
			AmountMinor: intent.Amount.MinorUnits(), Payee: &payee,
		})
		if err != nil {
			return SubmitIntentResult{}, fmt.Errorf("evaluating the organisation's spending policy: %w", err)
		}
		if decision.Outcome == policy.Deny {
			s.Audit.record(ctx, intent.OrgID, AuditPaymentDeniedByPolicy, "intent", string(intent.Key), map[string]string{
				"decision": decision.Outcome.String(), "policy_version": fmt.Sprint(decision.Version),
			})
			return SubmitIntentResult{}, &PolicyDeniedError{Decision: decision}
		}
		// Allow and Escalate both proceed to be saved, so both record
		// which version decided them - not just Deny/Escalate's audit
		// entries above.
		intent.PolicyVersion = &decision.Version
		escalate = decision.Outcome == policy.Escalate
	}

	_, created, err := s.Intents.Save(ctx, intent)
	if err != nil {
		return SubmitIntentResult{}, err
	}

	// Re-checked after Save: a concurrent fresh submission of this
	// same brand-new intent may have already appended PROPOSED between
	// the check above and this Save.
	chain, err = s.Events.Load(ctx, intent.OrgID, intent.Key)
	if err != nil {
		return SubmitIntentResult{}, err
	}
	if len(chain) > 0 {
		return SubmitIntentResult{Key: intent.Key, State: domain.State(chain[len(chain)-1].Type), Outcome: OutcomeDeduplicated}, nil
	}

	// No history yet: either brand new, or a run started moments ago
	// that hasn't recorded anything - starting attaches to that run.
	if err := s.Starter.Start(ctx, intent, escalate); err != nil {
		return SubmitIntentResult{}, err
	}
	// The escalation audit record and approver emails fire only once
	// the workflow has actually started waiting on them - not
	// earlier, when Save or Start could still fail and leave both for
	// a payment that never began.
	if escalate {
		s.Audit.record(ctx, intent.OrgID, AuditPaymentEscalated, "intent", string(intent.Key), map[string]string{
			"policy_version": fmt.Sprint(*intent.PolicyVersion),
		})
		if err := s.notifyApprovers(ctx, intent); err != nil {
			return SubmitIntentResult{}, fmt.Errorf("notifying approvers: %w", err)
		}
	}
	outcome := OutcomeStarted
	if !created {
		outcome = OutcomeAttached
	}
	return SubmitIntentResult{Key: intent.Key, State: domain.PROPOSED, Outcome: outcome}, nil
}

// buildIntent validates a submission into an Intent, naming the field
// at fault.
func buildIntent(cmd SubmitIntentCommand, treasury domain.TreasuryID) (domain.Intent, error) {
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
	intent, err := domain.NewIntent(cmd.OrgID, treasury, cmd.Payee, amount.MinorUnits(), string(currency), cmd.Reference, cmd.Reason)
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

// notifyApprovers emails every owner/admin that intent needs their
// approval. Notify-only, like passwordChangedEmail: the recipients
// are already authenticated members, who decide from the dashboard,
// so no one-time link is minted. Nil-safe: with no Members/Emails
// configured, nothing is sent.
func (s *SubmitIntent) notifyApprovers(ctx context.Context, intent domain.Intent) error {
	if s.Members == nil || s.Emails == nil {
		return nil
	}
	members, err := s.Members.ListMembers(ctx, intent.OrgID)
	if err != nil {
		return err
	}
	params := map[string]string{
		"payee":  intent.Payee,
		"amount": fmt.Sprintf("%s %s", intent.Amount.Currency(), intent.Amount.MajorUnitsString()),
	}
	for _, m := range members {
		if m.Role != domain.Owner && m.Role != domain.Admin {
			continue
		}
		if _, err := s.Emails.Send(ctx, Email{
			OrgID: intent.OrgID, UserID: m.User.ID, To: m.User.Email,
			Template: TemplateEscalationCreated, Params: params,
		}); err != nil {
			return err
		}
	}
	return nil
}
