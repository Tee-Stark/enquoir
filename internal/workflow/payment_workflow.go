package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// TaskQueue is the task queue PaymentWorkflow and its activities run
// on. Shared by the worker and anything that starts a payment.
const TaskQueue = "enquoir-payments"

// PaymentInput is a payment workflow's input. MaxAmountMinorUnits is
// supplied by whoever starts the workflow, not read inside it, so the
// workflow stays deterministic no matter where that configuration
// value came from.
type PaymentInput struct {
	Intent              domain.Intent
	MaxAmountMinorUnits int64
}

// PaymentResult is what a payment workflow run resolves to.
type PaymentResult struct {
	State         domain.State
	TransferID    string
	FailureReason string
}

const pollInterval = 2 * time.Second

var activityOptions = workflow.ActivityOptions{
	StartToCloseTimeout: 30 * time.Second,
	// A modest automatic retry budget covers a worker that dies with
	// no chance to report anything back - Temporal just redispatches
	// the activity once another worker is available. That is safe
	// specifically because Rail.CreateTransfer is idempotent on the
	// intent's key: a retried create either succeeds fresh or resolves
	// to the transfer the first attempt already made. Once our own
	// code has classified an outcome (Rejected/Ambiguous, tagged
	// NonRetryable below), Temporal stops retrying and the workflow
	// decides what happens next instead - that half is what "No blind
	// retries" is actually about.
	RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3},
}

// PaymentWorkflow takes one Intent from PROPOSED to APPLIED, FAILED or
// BLOCKED. The intent's key is this workflow's ID: starting the same
// intent twice attaches to the run already in progress instead of
// paying it twice.
func PaymentWorkflow(ctx workflow.Context, in PaymentInput) (PaymentResult, error) {
	ctx = workflow.WithActivityOptions(ctx, activityOptions)

	var intent domain.Intent
	if err := execActivity(ctx, activities().SaveIntent, in.Intent).Get(ctx, &intent); err != nil {
		return PaymentResult{}, fmt.Errorf("saving intent: %w", err)
	}

	state := domain.PROPOSED
	if err := appendEvent(ctx, intent.Key, string(state), ""); err != nil {
		return PaymentResult{}, err
	}

	if intent.Amount.MinorUnits() > in.MaxAmountMinorUnits {
		reason := fmt.Sprintf("amount %d exceeds maximum %d minor units", intent.Amount.MinorUnits(), in.MaxAmountMinorUnits)
		if err := moveTo(ctx, &state, intent.Key, domain.BLOCKED, reason); err != nil {
			return PaymentResult{}, err
		}
		return PaymentResult{State: domain.BLOCKED}, nil
	}

	if err := moveTo(ctx, &state, intent.Key, domain.AUTHORIZED, ""); err != nil {
		return PaymentResult{}, err
	}

	return authorize(ctx, &state, intent)
}

// authorize sends the intent to the rail and waits for a result. It
// always moves to IN_FLIGHT, recording that event before Rail is ever
// called, from either AUTHORIZED (first attempt) or UNKNOWN (retrying
// after confirming the previous attempt never reached the rail).
func authorize(ctx workflow.Context, state *domain.State, intent domain.Intent) (PaymentResult, error) {
	if err := moveTo(ctx, state, intent.Key, domain.IN_FLIGHT, ""); err != nil {
		return PaymentResult{}, err
	}

	var transfer app.Transfer
	err := execActivity(ctx, activities().CreateTransfer, intent).Get(ctx, &transfer)
	if err != nil {
		return handleCreateError(ctx, state, intent, err)
	}

	return pollUntilTerminal(ctx, state, intent, transfer)
}

// handleCreateError resolves a failed CreateTransfer call into the
// right next state. Only an explicit, classified rejection ends the
// intent at FAILED. Everything else - a classified ambiguous outcome,
// but just as much a bare activity failure with no classification at
// all, such as a worker that died mid-call and never got to report
// anything - is treated the same way: move to UNKNOWN and look the
// transfer up by request ID before anything else happens. Confirmed
// absent, it is safe to retry with the same request ID; found, the
// rail already has it and no second create is ever sent. Unclassified
// failures must resolve exactly like a classified ambiguous one,
// because that is exactly what they are: an outcome this code cannot
// tell apart from one.
func handleCreateError(ctx workflow.Context, state *domain.State, intent domain.Intent, err error) (PaymentResult, error) {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == errTypeRejected {
		if err := moveTo(ctx, state, intent.Key, domain.FAILED, appErr.Error()); err != nil {
			return PaymentResult{}, err
		}
		return PaymentResult{State: domain.FAILED, FailureReason: appErr.Error()}, nil
	}

	if err := moveTo(ctx, state, intent.Key, domain.UNKNOWN, err.Error()); err != nil {
		return PaymentResult{}, err
	}

	var found FindTransferResult
	if err := execActivity(ctx, activities().FindTransferByRequestID, string(intent.Key)).Get(ctx, &found); err != nil {
		return PaymentResult{}, err
	}
	if !found.Found {
		return authorize(ctx, state, intent)
	}

	if err := moveTo(ctx, state, intent.Key, domain.IN_FLIGHT, "resolved via lookup after an ambiguous create"); err != nil {
		return PaymentResult{}, err
	}
	return pollUntilTerminal(ctx, state, intent, found.Transfer)
}

// pollUntilTerminal must be entered with state already IN_FLIGHT. It
// polls the rail until the transfer reaches a terminal status, never
// treating SENT as final.
func pollUntilTerminal(ctx workflow.Context, state *domain.State, intent domain.Intent, transfer app.Transfer) (PaymentResult, error) {
	for {
		switch transfer.Status {
		case app.TransferPaid:
			if err := moveTo(ctx, state, intent.Key, domain.APPLIED, transfer.ID); err != nil {
				return PaymentResult{}, err
			}
			return PaymentResult{State: domain.APPLIED, TransferID: transfer.ID}, nil

		case app.TransferCancelled:
			if err := moveTo(ctx, state, intent.Key, domain.FAILED, transfer.FailureReason); err != nil {
				return PaymentResult{}, err
			}
			return PaymentResult{State: domain.FAILED, TransferID: transfer.ID, FailureReason: transfer.FailureReason}, nil

		default: // SENT: in flight, not final.
			if err := workflow.Sleep(ctx, pollInterval); err != nil {
				return PaymentResult{}, err
			}
			if err := execActivity(ctx, activities().GetTransfer, transfer.ID).Get(ctx, &transfer); err != nil {
				return PaymentResult{}, err
			}
		}
	}
}

// moveTo validates the transition through the domain's own state
// machine before recording it, so the workflow can never emit an
// event for a move the domain itself would reject.
func moveTo(ctx workflow.Context, state *domain.State, key domain.IntentKey, next domain.State, payload string) error {
	if err := state.Transition(next); err != nil {
		return fmt.Errorf("workflow: %w", err)
	}
	if err := appendEvent(ctx, key, string(next), payload); err != nil {
		return err
	}
	*state = next
	return nil
}

func appendEvent(ctx workflow.Context, key domain.IntentKey, eventType, payload string) error {
	in := AppendEventInput{IntentKey: key, Type: eventType, Payload: payload}
	return execActivity(ctx, activities().AppendEvent, in).Get(ctx, nil)
}

func execActivity(ctx workflow.Context, activity any, args ...any) workflow.Future {
	return workflow.ExecuteActivity(ctx, activity, args...)
}

// activities returns a nil *Activities purely so the workflow can refer
// to its methods by value - Temporal resolves the call by the method's
// registered name, not by dereferencing this pointer.
func activities() *Activities {
	return nil
}
