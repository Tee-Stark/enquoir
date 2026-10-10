// Command demo submits one Intent against the real Airwallex sandbox,
// runs its own in-process worker to execute the payment workflow,
// moves the resulting transfer to PAID through the simulator, and
// prints the final state and full event history.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"enquoir/internal/adapters/airwallex"
	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/app"
	"enquoir/internal/domain"
	enquoirworkflow "enquoir/internal/workflow"
)

const (
	demoAmountMinorUnits = 1000 // $10.00
	defaultMaxMinorUnits = 1_000_000_00
	findTimeout          = 30 * time.Second
	resultTimeout        = 60 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	databaseURL := requireEnv(logger, "DATABASE_URL")
	awxBaseURL := requireEnv(logger, "AWX_BASE_URL")
	awxClientID := requireEnv(logger, "AWX_CLIENT_ID")
	awxAPIKey := requireEnv(logger, "AWX_API_KEY")
	temporalAddress := os.Getenv("TEMPORAL_ADDRESS")
	if temporalAddress == "" {
		temporalAddress = "127.0.0.1:7233"
	}
	maxMinorUnits := int64(defaultMaxMinorUnits)
	if v := os.Getenv("MAX_PAYMENT_MINOR_UNITS"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			logger.Error("invalid MAX_PAYMENT_MINOR_UNITS", "value", v, "error", err)
			os.Exit(1)
		}
		maxMinorUnits = parsed
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	pool, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		logger.Error("connecting to Postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	intents := postgres.NewIntentRepository(pool)
	events := postgres.NewEventLog(pool)

	rail, err := airwallex.NewClient(awxBaseURL, awxClientID, awxAPIKey, httpClient, clock.System{})
	if err != nil {
		logger.Error("building Airwallex client", "error", err)
		os.Exit(1)
	}
	simulator, err := airwallex.NewSimulator(awxBaseURL, awxClientID, awxAPIKey, httpClient, clock.System{})
	if err != nil {
		logger.Error("building Airwallex simulator", "error", err)
		os.Exit(1)
	}

	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		logger.Error("connecting to Temporal", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	w := worker.New(temporalClient, enquoirworkflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(enquoirworkflow.PaymentWorkflow)
	w.RegisterActivity(&enquoirworkflow.Activities{Intents: intents, Events: events, Rail: rail})
	if err := w.Start(); err != nil {
		logger.Error("starting in-process worker", "error", err)
		os.Exit(1)
	}
	defer w.Stop()

	beneficiaryID, err := simulator.EnsureBeneficiary(ctx)
	if err != nil {
		logger.Error("ensuring test beneficiary", "error", err)
		os.Exit(1)
	}

	reference := fmt.Sprintf("demo-%d", time.Now().UnixNano())
	intent, err := domain.NewIntent(beneficiaryID, demoAmountMinorUnits, "USD", reference, "enquoir demo")
	if err != nil {
		logger.Error("building intent", "error", err)
		os.Exit(1)
	}
	logger.Info("submitting intent", "intent_key", intent.Key, "amount", intent.Amount.MajorUnitsString())

	opts := client.StartWorkflowOptions{ID: string(intent.Key), TaskQueue: enquoirworkflow.TaskQueue}
	run, err := temporalClient.ExecuteWorkflow(ctx, opts, enquoirworkflow.PaymentWorkflow, enquoirworkflow.PaymentInput{
		Intent:              intent,
		MaxAmountMinorUnits: maxMinorUnits,
	})
	if err != nil {
		logger.Error("starting workflow", "error", err)
		os.Exit(1)
	}

	transfer, err := awaitTransfer(ctx, rail, string(intent.Key))
	if err != nil {
		logger.Error("waiting for the rail to create the transfer", "error", err)
		os.Exit(1)
	}
	logger.Info("transfer created", "transfer_id", transfer.ID, "status", transfer.Status)

	if err := simulator.TransitionTransfer(ctx, transfer.ID, app.TransferSent); err != nil {
		logger.Warn("transitioning transfer to SENT", "error", err)
	}
	if err := simulator.TransitionTransfer(ctx, transfer.ID, app.TransferPaid); err != nil {
		logger.Error("transitioning transfer to PAID", "error", err)
		os.Exit(1)
	}
	logger.Info("transfer moved to PAID through the simulator")

	resultCtx, cancel := context.WithTimeout(ctx, resultTimeout)
	defer cancel()

	var result enquoirworkflow.PaymentResult
	if err := run.Get(resultCtx, &result); err != nil {
		logger.Error("waiting for workflow result", "error", err)
		os.Exit(1)
	}

	chain, err := events.Load(ctx, intent.Key)
	if err != nil {
		logger.Error("loading event history", "error", err)
		os.Exit(1)
	}
	if err := chain.Verify(); err != nil {
		logger.Error("event chain failed to verify", "error", err)
		os.Exit(1)
	}

	fmt.Printf("\nFinal state: %s\n", result.State)
	if result.TransferID != "" {
		fmt.Printf("Transfer ID: %s\n", result.TransferID)
	}
	fmt.Println("\nEvent history:")
	for _, ev := range chain {
		fmt.Printf("  %d. %-10s %s\n", ev.SequenceNumber, ev.Type, ev.Payload)
	}
}

// awaitTransfer polls the rail for the transfer the workflow is
// creating, since the demo needs its ID before it can drive it to PAID
// through the simulator.
func awaitTransfer(ctx context.Context, rail app.Rail, requestID string) (app.Transfer, error) {
	deadline := time.Now().Add(findTimeout)
	for {
		transfer, ok, err := rail.FindTransferByRequestID(ctx, requestID)
		if err != nil {
			return app.Transfer{}, err
		}
		if ok {
			return transfer, nil
		}
		if time.Now().After(deadline) {
			return app.Transfer{}, fmt.Errorf("no transfer found for request id %s within %s", requestID, findTimeout)
		}
		time.Sleep(time.Second)
	}
}

func requireEnv(logger *slog.Logger, name string) string {
	v := os.Getenv(name)
	if v == "" {
		logger.Error("missing required environment variable", "name", name)
		os.Exit(1)
	}
	return v
}
