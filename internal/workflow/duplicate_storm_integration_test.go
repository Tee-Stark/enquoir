//go:build integration

package workflow

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

const duplicateStormTaskQueue = "enquoir-payments-duplicate-storm-test"

// This runs against the real Temporal dev server - the attach-to-
// running-workflow behaviour under real concurrency (as opposed to
// one sequential pair of calls) depends on the server's own
// workflow-ID deduplication, same as the Phase 2 attach test. The
// Rail is still a fake: this proves the workflow layer's own
// behaviour under concurrent submission, not the sandbox.
func TestPaymentWorkflow_DuplicateStormResultsInExactlyOneCreate(t *testing.T) {
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		address = "127.0.0.1:7233"
	}

	c, err := client.Dial(client.Options{HostPort: address})
	require.NoError(t, err)
	defer c.Close()

	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_storm", Status: app.TransferSent}, nil
		},
		getFunc: func(ctx context.Context, transferID string) (app.Transfer, error) {
			return app.Transfer{ID: transferID, Status: app.TransferPaid}, nil
		},
	}

	w := worker.New(c, duplicateStormTaskQueue, worker.Options{})
	w.RegisterWorkflow(PaymentWorkflow)
	w.RegisterActivity(&Activities{
		Intents: newFakeIntentRepository(),
		Events:  newFakeEventLog(),
		Rail:    rail,
	})
	require.NoError(t, w.Start())
	defer w.Stop()

	intent, err := domain.NewIntent(
		"beneficiary-1", 10050, "USD",
		"storm-"+time.Now().Format("20060102150405.000000000"),
		"duplicate storm test",
	)
	require.NoError(t, err)

	input := PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax}
	opts := client.StartWorkflowOptions{ID: string(intent.Key), TaskQueue: duplicateStormTaskQueue}

	const concurrency = 20
	runIDs := make([]string, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run, err := c.ExecuteWorkflow(context.Background(), opts, PaymentWorkflow, input)
			if err != nil {
				errs[i] = err
				return
			}
			runIDs[i] = run.GetRunID()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "ExecuteWorkflow call %d", i)
	}
	for i, runID := range runIDs {
		require.Equal(t, runIDs[0], runID, "call %d attached to a different run than call 0", i)
	}

	handle := c.GetWorkflow(context.Background(), string(intent.Key), runIDs[0])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result PaymentResult
	require.NoError(t, handle.Get(ctx, &result))
	require.Equal(t, domain.APPLIED, result.State)

	require.Equal(t, 1, rail.calls(), fmt.Sprintf("%d concurrent submissions of the same intent must result in exactly one CreateTransfer call", concurrency))
}
