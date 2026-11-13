package httpapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"enquoir/internal/app"
	"enquoir/internal/domain"
	enquoirworkflow "enquoir/internal/workflow"
)

// TemporalPaymentStarter implements app.PaymentStarter against a real
// Temporal client. Starting the same intent twice attaches to the run
// already in progress - that is the server's own behaviour for a
// workflow ID that is still running, nothing extra to implement here.
type TemporalPaymentStarter struct {
	Client              client.Client
	MaxAmountMinorUnits int64
}

func (s *TemporalPaymentStarter) Start(ctx context.Context, intent domain.Intent, escalate bool) error {
	opts := client.StartWorkflowOptions{
		ID:        string(intent.Key),
		TaskQueue: enquoirworkflow.TaskQueue,
	}
	_, err := s.Client.ExecuteWorkflow(ctx, opts, enquoirworkflow.PaymentWorkflow, enquoirworkflow.PaymentInput{
		Intent:              intent,
		MaxAmountMinorUnits: s.MaxAmountMinorUnits,
		Escalate:            escalate,
	})
	return err
}

// SignalApproval sends an approve/reject decision to the payment
// workflow waiting for it in AWAITING_APPROVAL.
func (s *TemporalPaymentStarter) SignalApproval(ctx context.Context, key domain.IntentKey, approved bool, decidedBy string) error {
	return s.Client.SignalWorkflow(ctx, string(key), "", enquoirworkflow.EscalationSignalName,
		enquoirworkflow.EscalationSignal{Approved: approved, DecidedBy: decidedBy})
}

// registerTimeout bounds how long a registration request waits on its
// workflow. Timing out is safe to retry: the next request attaches to
// the same run, or finds the finished beneficiary by its key.
const registerTimeout = 60 * time.Second

// TemporalBeneficiaryRegistrar implements app.BeneficiaryRegistrar by
// starting, or attaching to, BeneficiaryWorkflow and waiting for the
// rail's ID.
type TemporalBeneficiaryRegistrar struct {
	Client client.Client
}

func (r *TemporalBeneficiaryRegistrar) Register(ctx context.Context, b domain.Beneficiary) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()

	opts := client.StartWorkflowOptions{
		ID:        enquoirworkflow.BeneficiaryWorkflowID(b.Key),
		TaskQueue: enquoirworkflow.TaskQueue,
	}
	run, err := r.Client.ExecuteWorkflow(ctx, opts, enquoirworkflow.BeneficiaryWorkflow, b)
	if err != nil {
		return "", err
	}

	var id string
	if err := run.Get(ctx, &id); err != nil {
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == enquoirworkflow.ErrTypeRejected {
			return "", fmt.Errorf("%s: %w", appErr.Message(), app.ErrRejected)
		}
		return "", err
	}
	return id, nil
}
