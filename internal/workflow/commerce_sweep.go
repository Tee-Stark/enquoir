package workflow

import (
	"context"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// CommerceSweepWorkflowID is the one sweep workflow: starting it again
// while it runs attaches.
const CommerceSweepWorkflowID = "commerce-execution-sweep"

const (
	commerceSweepInterval = time.Minute
	// commerceSweepsPerRun bounds one run's history before it continues
	// as new.
	commerceSweepsPerRun = 60
)

var sweepActivityOptions = workflow.ActivityOptions{
	StartToCloseTimeout: time.Minute,
	RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3, MaximumInterval: 30 * time.Second},
}

// SweepInput is the sweep's input: sweep Iterations times, a minute
// apart, then stop - or, Forever, continue as new and go on.
type SweepInput struct {
	Iterations int
	Forever    bool
}

// CommerceSweepWorkflow presumes unknown every commerce execution left
// pending past its report deadline - the merchant may have crashed
// mid-charge - so it is reconciled, never retried blind. It survives
// worker crashes: Temporal resumes it where it was.
func CommerceSweepWorkflow(ctx workflow.Context, in SweepInput) error {
	actx := workflow.WithActivityOptions(ctx, sweepActivityOptions)
	for i := 0; i < in.Iterations; i++ {
		var swept int
		if err := workflow.ExecuteActivity(actx, activities().SweepCommerceExecutions).Get(ctx, &swept); err != nil {
			workflow.GetLogger(ctx).Warn("sweeping commerce executions failed; trying again next interval", "error", err)
		}
		if i < in.Iterations-1 || in.Forever {
			if err := workflow.Sleep(ctx, commerceSweepInterval); err != nil {
				return err
			}
		}
	}
	if in.Forever {
		return workflow.NewContinueAsNewError(ctx, CommerceSweepWorkflow, in)
	}
	return nil
}

// SweepCommerceExecutions is the sweep's activity: the time is read
// here, never in the workflow.
func (a *Activities) SweepCommerceExecutions(ctx context.Context) (int, error) {
	return a.ExecutionSweep.Run(ctx)
}

// StartCommerceSweep starts the sweep, or attaches to the one running.
func StartCommerceSweep(ctx context.Context, c client.Client) error {
	_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: CommerceSweepWorkflowID, TaskQueue: TaskQueue},
		CommerceSweepWorkflow, SweepInput{Iterations: commerceSweepsPerRun, Forever: true})
	return err
}
