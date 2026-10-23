package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"enquoir/internal/domain"
)

// BeneficiaryWorkflowID is the workflow ID a beneficiary registers
// under: its key, prefixed so it can never collide with a payment
// workflow's ID (an intent's key) in the same namespace. Registering
// the same beneficiary twice concurrently attaches to one run.
func BeneficiaryWorkflowID(key domain.BeneficiaryKey) string {
	return "beneficiary-" + string(key)
}

// createBeneficiaryOptions allow exactly one attempt. Unlike
// CreateTransfer, the rail's beneficiary create takes no request_id, so
// an automatic retry could register the same payee twice. Every failed
// attempt that isn't a clear rejection goes to a lookup instead.
var createBeneficiaryOptions = workflow.ActivityOptions{
	StartToCloseTimeout: 30 * time.Second,
	RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
}

// BeneficiaryWorkflow registers b on the rail and returns the rail's ID
// for it, looking it up by key first so a re-sent registration returns
// the existing beneficiary instead of creating a second one. A clear
// rejection fails the workflow with an ErrTypeRejected application
// error.
func BeneficiaryWorkflow(ctx workflow.Context, b domain.Beneficiary) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, activityOptions)

	for {
		var found FindBeneficiaryResult
		if err := execActivity(ctx, activities().FindBeneficiary, b.Key).Get(ctx, &found); err != nil {
			return "", fmt.Errorf("looking up beneficiary: %w", err)
		}
		if found.Found {
			return found.ID, nil
		}

		var id string
		createCtx := workflow.WithActivityOptions(ctx, createBeneficiaryOptions)
		err := execActivity(createCtx, activities().CreateBeneficiary, b).Get(createCtx, &id)
		if err == nil {
			return id, nil
		}

		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == ErrTypeRejected {
			return "", err
		}
		// Anything else is an unclear outcome: loop back to the lookup,
		// which decides whether the create landed before any retry.
	}
}
