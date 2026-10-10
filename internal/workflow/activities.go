// Package workflow holds the payment workflow and the activities that
// do its I/O. Workflow code stays deterministic - no clock, randomness,
// network or database call ever happens there; those live here, in
// Activities, instead.
package workflow

import (
	"context"
	"errors"

	"go.temporal.io/sdk/temporal"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// Activity error types, carried across the activity boundary via
// temporal.ApplicationError since a plain errors.Is on app.ErrRejected/
// app.ErrAmbiguous doesn't survive that boundary.
const (
	errTypeRejected  = "Rejected"
	errTypeAmbiguous = "Ambiguous"
)

// Activities wraps the ports that must run as Temporal activities.
type Activities struct {
	Intents app.IntentRepository
	Events  app.EventLog
	Rail    app.Rail
}

func (a *Activities) SaveIntent(ctx context.Context, intent domain.Intent) (domain.Intent, error) {
	return a.Intents.Save(ctx, intent)
}

// AppendEventInput is AppendEvent's input, since Temporal activities
// take at most one argument that isn't a context.
type AppendEventInput struct {
	IntentKey domain.IntentKey
	Type      string
	Payload   string
}

func (a *Activities) AppendEvent(ctx context.Context, in AppendEventInput) (domain.Event, error) {
	return a.Events.Append(ctx, in.IntentKey, in.Type, in.Payload)
}

func (a *Activities) CreateTransfer(ctx context.Context, intent domain.Intent) (app.Transfer, error) {
	transfer, err := a.Rail.CreateTransfer(ctx, intent)
	if err != nil {
		return app.Transfer{}, toApplicationError(err)
	}
	return transfer, nil
}

func (a *Activities) GetTransfer(ctx context.Context, transferID string) (app.Transfer, error) {
	transfer, err := a.Rail.GetTransfer(ctx, transferID)
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

func (a *Activities) FindTransferByRequestID(ctx context.Context, requestID string) (FindTransferResult, error) {
	transfer, ok, err := a.Rail.FindTransferByRequestID(ctx, requestID)
	if err != nil {
		return FindTransferResult{}, toApplicationError(err)
	}
	return FindTransferResult{Transfer: transfer, Found: ok}, nil
}

// toApplicationError tags the two outcomes the workflow must branch on
// so they survive the activity boundary; anything else is left as-is
// for Temporal's default handling, since the workflow has no specific
// recovery for an error it doesn't recognise.
func toApplicationError(err error) error {
	switch {
	case errors.Is(err, app.ErrRejected):
		return temporal.NewApplicationErrorWithOptions(err.Error(), errTypeRejected, temporal.ApplicationErrorOptions{NonRetryable: true})
	case errors.Is(err, app.ErrAmbiguous):
		return temporal.NewApplicationErrorWithOptions(err.Error(), errTypeAmbiguous, temporal.ApplicationErrorOptions{NonRetryable: true})
	default:
		return err
	}
}
