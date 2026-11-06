//go:build integration

package workflow

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"enquoir/internal/domain"
)

const attachTestTaskQueue = "enquoir-payments-attach-test"

// This behaviour depends on the real server's own workflow-ID
// deduplication, which the lightweight test suite used everywhere
// else in this package does not model - hence a real Temporal dev
// server (the one Phase 1's docker-compose brings up) instead of a
// fake Rail and in-memory repositories.
func TestPaymentWorkflow_StartingSameIntentTwiceAttachesToRunningWorkflow(t *testing.T) {
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		address = "127.0.0.1:7233"
	}

	c, err := client.Dial(client.Options{HostPort: address})
	require.NoError(t, err)
	// Cleanups run last-registered-first, so registering Close first
	// keeps the client open for the terminate below. A deferred Close
	// would run before any cleanup and silently break the terminate,
	// leaving a live payment workflow behind.
	t.Cleanup(c.Close)

	intent, err := domain.NewIntent(
		"beneficiary-1", 10050, "USD",
		"attach-test-"+time.Now().Format("20060102150405.000000000"),
		"attach test",
	)
	require.NoError(t, err)

	input := PaymentInput{Intent: intent, MaxAmountMinorUnits: 1_000_000_00}
	// A task queue no real worker polls: this test only needs the
	// server's workflow-ID dedup, never an execution. On the real queue,
	// a running `make worker` would pick the workflow up and try to pay
	// it through the sandbox.
	opts := client.StartWorkflowOptions{ID: string(intent.Key), TaskQueue: attachTestTaskQueue}

	ctx := context.Background()
	run1, err := c.ExecuteWorkflow(ctx, opts, PaymentWorkflow, input)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := c.TerminateWorkflow(context.Background(), run1.GetID(), "", "integration test cleanup")
		require.NoError(t, err, "terminating the test workflow")
	})

	run2, err := c.ExecuteWorkflow(ctx, opts, PaymentWorkflow, input)
	require.NoError(t, err)

	require.Equal(t, run1.GetRunID(), run2.GetRunID(),
		"starting the same intent twice must attach to the already-running workflow, not start a second one")
}
