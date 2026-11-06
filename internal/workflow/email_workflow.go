package workflow

import (
	"context"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"enquoir/internal/app"
)

// emailRetryWindow bounds a send's retries inside Resend's 24-hour
// idempotency window: within it, a retry can never deliver a second
// copy; past it, it could.
const emailRetryWindow = 23 * time.Hour

var deliverEmailOptions = workflow.ActivityOptions{
	StartToCloseTimeout:    time.Minute,
	ScheduleToCloseTimeout: emailRetryWindow,
	RetryPolicy: &temporal.RetryPolicy{
		InitialInterval:    10 * time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    time.Hour,
	},
}

// EmailWorkflowID is a queued email's send workflow: starting it again
// while it runs attaches.
func EmailWorkflowID(emailID string) string { return "email-" + emailID }

// SendEmailWorkflow delivers one queued email, retrying unclear
// failures, then records it failed if it never goes through. Only the
// email's ID crosses the workflow boundary: no address, link or token
// ever enters Temporal's history.
func SendEmailWorkflow(ctx workflow.Context, emailID string) error {
	deliverCtx := workflow.WithActivityOptions(ctx, deliverEmailOptions)
	err := workflow.ExecuteActivity(deliverCtx, activities().DeliverEmail, emailID).Get(ctx, nil)
	if err == nil {
		return nil
	}
	giveUpCtx := workflow.WithActivityOptions(ctx, activityOptions)
	return workflow.ExecuteActivity(giveUpCtx, activities().GiveUpEmail, GiveUpEmailInput{EmailID: emailID, Reason: err.Error()}).Get(ctx, nil)
}

// GiveUpEmailInput is GiveUpEmail's input.
type GiveUpEmailInput struct {
	EmailID string
	Reason  string
}

func (a *Activities) DeliverEmail(ctx context.Context, emailID string) error {
	return a.EmailDelivery.Run(ctx, emailID)
}

func (a *Activities) GiveUpEmail(ctx context.Context, in GiveUpEmailInput) error {
	reason := in.Reason
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return a.EmailGiveUp.Run(ctx, in.EmailID, reason)
}

// EmailDispatcher starts emails' send workflows: app.EmailDispatcher
// over a Temporal client.
type EmailDispatcher struct{ Client client.Client }

var _ app.EmailDispatcher = (*EmailDispatcher)(nil)

func (d *EmailDispatcher) Dispatch(ctx context.Context, emailID string) error {
	_, err := d.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: EmailWorkflowID(emailID), TaskQueue: TaskQueue}, SendEmailWorkflow, emailID)
	return err
}
