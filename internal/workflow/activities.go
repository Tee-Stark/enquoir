// Package workflow holds the payment workflow and the activities that
// do its I/O. Workflow code stays deterministic - no clock, randomness,
// network or database call ever happens there; those live here, in
// Activities, instead.
package workflow

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"enquoir/internal/app"
	"enquoir/internal/chaos"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

// Activity error types, carried across the activity boundary via
// temporal.ApplicationError since a plain errors.Is on app.ErrRejected/
// app.ErrAmbiguous doesn't survive that boundary. ErrTypeRejected is
// exported so whoever starts a workflow can recognise a rejection in
// the workflow's own failure, too.
const (
	ErrTypeRejected           = "Rejected"
	errTypeAmbiguous          = "Ambiguous"
	errTypeDuplicateTransfers = "DuplicateTransfers"
)

// Activities wraps the ports that must run as Temporal activities.
// Every rail call is recorded as a rail_call attempt in Attempts:
// started before the call leaves, finished when it returns, so a call
// whose worker dies mid-flight is still on the record.
type Activities struct {
	Intents       app.IntentRepository
	Events        app.EventLog
	Rail          app.Rail
	Beneficiaries app.BeneficiaryRegistry

	Attempts app.AttemptLog
	IDs      app.IDGenerator
	Clock    app.Clock

	EmailDelivery *app.DeliverEmail
	EmailGiveUp   *app.GiveUpEmail

	// ExecutionSweep presumes overdue commerce executions unknown.
	ExecutionSweep *commerceapp.SweepExecutions

	// Policies re-evaluates a payment approved out of AWAITING_APPROVAL,
	// since limits may have moved since it was escalated.
	Policies policy.Store
}

// RecheckPolicy evaluates intent against the organisation's current
// policy and usage, exactly as submission did - approving an
// escalated payment must never let through on a decision now stale.
// Nil Policies always allows, matching SubmitIntent/SubmitCheckout's
// own nil-safety.
func (a *Activities) RecheckPolicy(ctx context.Context, intent domain.Intent) (policy.Decision, error) {
	if a.Policies == nil {
		return policy.Decision{Outcome: policy.Allow}, nil
	}
	payee := intent.Payee
	return a.Policies.EvaluateAndReserve(ctx, intent.OrgID, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: intent.Amount.Currency(),
		AmountMinor: intent.Amount.MinorUnits(), Payee: &payee,
	})
}

// TransferRef names a transfer and the intent it pays.
type TransferRef struct {
	OrgID      domain.OrgID
	IntentKey  domain.IntentKey
	TransferID string
}

// IntentRef names an intent.
type IntentRef struct {
	OrgID     domain.OrgID
	IntentKey domain.IntentKey
}

// BeneficiaryRef names a beneficiary.
type BeneficiaryRef struct {
	OrgID domain.OrgID
	Key   domain.BeneficiaryKey
}

// maxRecordedMessage bounds an error message kept on an attempt.
const maxRecordedMessage = 1000

// track records one rail call around call: started before, finished
// after. Failing to record the start fails the activity before the
// rail is called - safe, since nothing was sent. Failing to record the
// finish is logged and ignored: the call happened, and the event log,
// not the attempt log, is what money safety rests on.
func (a *Activities) track(ctx context.Context, base app.Attempt, call func() (app.AttemptFinish, error)) error {
	base.ID = a.IDs.NewID("att")
	base.Kind = app.KindRailCall
	base.StartedAt = a.Clock.Now()
	if activity.IsActivity(ctx) {
		base.ActivityAttempt = int(activity.GetInfo(ctx).Attempt)
	}
	if err := a.Attempts.Start(ctx, base); err != nil {
		return fmt.Errorf("recording the %s attempt: %w", base.Operation, err)
	}

	finish, err := call()
	if err != nil {
		finish = failedFinish(err)
	}
	finish.FinishedAt = a.Clock.Now()
	if ferr := a.Attempts.Finish(ctx, base.OrgID, base.ID, finish); ferr != nil && activity.IsActivity(ctx) {
		activity.GetLogger(ctx).Warn("could not record a rail call's outcome", "attempt", base.ID, "error", ferr)
	}
	return err
}

func failedFinish(err error) app.AttemptFinish {
	f := app.AttemptFinish{Outcome: app.OutcomeError, ErrorMessage: err.Error()}
	if len(f.ErrorMessage) > maxRecordedMessage {
		f.ErrorMessage = f.ErrorMessage[:maxRecordedMessage]
	}
	var re *app.RailError
	if errors.As(err, &re) {
		f.HTTPStatus, f.ErrorCode = re.HTTPStatus, re.Code
	}
	switch {
	case errors.Is(err, app.ErrRejected):
		f.Outcome = app.OutcomeRejected
	case errors.Is(err, app.ErrAmbiguous):
		f.Outcome = app.OutcomeAmbiguous
	case errors.Is(err, app.ErrDuplicateTransfers):
		f.Outcome = app.OutcomeDuplicateTransfers
	}
	return f
}

func transferFinish(t app.Transfer) app.AttemptFinish {
	return app.AttemptFinish{Outcome: app.OutcomeOK, TransferID: t.ID, TransferStatus: string(t.Status)}
}

func (a *Activities) SaveIntent(ctx context.Context, intent domain.Intent) (domain.Intent, error) {
	stored, _, err := a.Intents.Save(ctx, intent)
	return stored, err
}

// AppendEventInput is AppendEvent's input, since Temporal activities
// take at most one argument that isn't a context.
type AppendEventInput struct {
	OrgID     domain.OrgID
	IntentKey domain.IntentKey
	Type      string
	Payload   string
}

func (a *Activities) AppendEvent(ctx context.Context, in AppendEventInput) (domain.Event, error) {
	// before_log: the IN_FLIGHT event is the one write every safety
	// guarantee here hangs on - "before the call" means before this
	// one specifically, not any of the others this same activity logs.
	if in.Type == string(domain.IN_FLIGHT) {
		chaos.KillAt(chaos.BeforeLog)
	}
	return a.Events.Append(ctx, in.OrgID, in.IntentKey, in.Type, in.Payload)
}

func (a *Activities) CreateTransfer(ctx context.Context, intent domain.Intent) (app.Transfer, error) {
	chaos.KillAt(chaos.AfterLogBeforeCall)

	var transfer app.Transfer
	err := a.track(ctx, app.Attempt{OrgID: intent.OrgID, Operation: app.OpCreateTransfer, IntentKey: intent.Key, RequestID: string(intent.Key)},
		func() (app.AttemptFinish, error) {
			// during_call fires inside the rail's HTTP transport (see
			// chaos.ArmTransport, wired in cmd/worker): the moment the
			// create request has fully left the process, before any
			// response is read.
			var err error
			transfer, err = a.Rail.CreateTransfer(ctx, intent)
			chaos.KillAt(chaos.AfterCallBeforeCommit)
			return transferFinish(transfer), err
		})
	if err != nil {
		return app.Transfer{}, toApplicationError(err)
	}
	return transfer, nil
}

func (a *Activities) GetTransfer(ctx context.Context, ref TransferRef) (app.Transfer, error) {
	var transfer app.Transfer
	err := a.track(ctx, app.Attempt{OrgID: ref.OrgID, Operation: app.OpGetTransfer, IntentKey: ref.IntentKey},
		func() (app.AttemptFinish, error) {
			var err error
			transfer, err = a.Rail.GetTransfer(ctx, ref.TransferID)
			return transferFinish(transfer), err
		})
	if err != nil {
		return app.Transfer{}, toApplicationError(err)
	}
	return transfer, nil
}

// FindTransferResult carries FindTransferByRequestID's result, since a
// bool second return doesn't survive the activity boundary cleanly.
type FindTransferResult struct {
	Transfer app.Transfer
	Found    bool
}

func (a *Activities) FindTransferByRequestID(ctx context.Context, ref IntentRef) (FindTransferResult, error) {
	var result FindTransferResult
	err := a.track(ctx, app.Attempt{OrgID: ref.OrgID, Operation: app.OpFindTransfer, IntentKey: ref.IntentKey, RequestID: string(ref.IntentKey)},
		func() (app.AttemptFinish, error) {
			transfer, ok, err := a.Rail.FindTransferByRequestID(ctx, string(ref.IntentKey))
			result = FindTransferResult{Transfer: transfer, Found: ok}
			if !ok {
				return app.AttemptFinish{Outcome: app.OutcomeNotFound}, err
			}
			return transferFinish(transfer), err
		})
	if err != nil {
		return FindTransferResult{}, toApplicationError(err)
	}
	return result, nil
}

func (a *Activities) CreateBeneficiary(ctx context.Context, b domain.Beneficiary) (string, error) {
	var id string
	err := a.track(ctx, app.Attempt{OrgID: b.OrgID, Operation: app.OpCreateBeneficiary, BeneficiaryKey: b.Key},
		func() (app.AttemptFinish, error) {
			var err error
			id, err = a.Beneficiaries.CreateBeneficiary(ctx, b)
			return app.AttemptFinish{Outcome: app.OutcomeOK}, err
		})
	if err != nil {
		return "", toApplicationError(err)
	}
	return id, nil
}

// FindBeneficiaryResult carries FindBeneficiary's result, for the same
// reason FindTransferResult exists.
type FindBeneficiaryResult struct {
	ID    string
	Found bool
}

func (a *Activities) FindBeneficiary(ctx context.Context, ref BeneficiaryRef) (FindBeneficiaryResult, error) {
	var result FindBeneficiaryResult
	err := a.track(ctx, app.Attempt{OrgID: ref.OrgID, Operation: app.OpFindBeneficiary, BeneficiaryKey: ref.Key},
		func() (app.AttemptFinish, error) {
			id, ok, err := a.Beneficiaries.FindBeneficiary(ctx, ref.Key)
			result = FindBeneficiaryResult{ID: id, Found: ok}
			if !ok {
				return app.AttemptFinish{Outcome: app.OutcomeNotFound}, err
			}
			return app.AttemptFinish{Outcome: app.OutcomeOK}, err
		})
	if err != nil {
		return FindBeneficiaryResult{}, toApplicationError(err)
	}
	return result, nil
}

// toApplicationError tags the outcomes the workflow must branch on
// so they survive the activity boundary; anything else is left as-is
// for Temporal's default handling, since the workflow has no specific
// recovery for an error it doesn't recognise.
func toApplicationError(err error) error {
	switch {
	case errors.Is(err, app.ErrRejected):
		return temporal.NewApplicationErrorWithOptions(err.Error(), ErrTypeRejected, temporal.ApplicationErrorOptions{NonRetryable: true})
	case errors.Is(err, app.ErrAmbiguous):
		return temporal.NewApplicationErrorWithOptions(err.Error(), errTypeAmbiguous, temporal.ApplicationErrorOptions{NonRetryable: true})
	case errors.Is(err, app.ErrDuplicateTransfers):
		return temporal.NewApplicationErrorWithOptions(err.Error(), errTypeDuplicateTransfers, temporal.ApplicationErrorOptions{NonRetryable: true})
	default:
		return err
	}
}
