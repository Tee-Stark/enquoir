package httpapi

import (
	"context"

	"go.temporal.io/sdk/client"

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

func (s *TemporalPaymentStarter) Start(ctx context.Context, intent domain.Intent) error {
	opts := client.StartWorkflowOptions{
		ID:        string(intent.Key),
		TaskQueue: enquoirworkflow.TaskQueue,
	}
	_, err := s.Client.ExecuteWorkflow(ctx, opts, enquoirworkflow.PaymentWorkflow, enquoirworkflow.PaymentInput{
		Intent:              intent,
		MaxAmountMinorUnits: s.MaxAmountMinorUnits,
	})
	return err
}
