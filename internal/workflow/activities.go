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
	"enquoir/internal/chaos"
	"enquoir/internal/domain"
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
type Activities struct {
	Intents       app.IntentRepository
	Events        app.EventLog
	Rail          app.Rail
	Beneficiaries app.BeneficiaryRegistry
}

func (a *Activities) SaveIntent(ctx context.Context, intent domain.Intent) (domain.Intent, error) {
	return a.Intents.Save(ctx, intent)
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

	// during_call fires inside the rail's HTTP transport (see
	// chaos.ArmTransport, wired in cmd/worker): the moment the create
	// request has fully left the process, before any response is read.
	transfer, err := a.Rail.CreateTransfer(ctx, intent)
	chaos.KillAt(chaos.AfterCallBeforeCommit)

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

func (a *Activities) CreateBeneficiary(ctx context.Context, b domain.Beneficiary) (string, error) {
	id, err := a.Beneficiaries.CreateBeneficiary(ctx, b)
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

func (a *Activities) FindBeneficiary(ctx context.Context, key domain.BeneficiaryKey) (FindBeneficiaryResult, error) {
	id, ok, err := a.Beneficiaries.FindBeneficiary(ctx, key)
	if err != nil {
		return FindBeneficiaryResult{}, toApplicationError(err)
	}
	return FindBeneficiaryResult{ID: id, Found: ok}, nil
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
