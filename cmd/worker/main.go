// Command worker runs Enquoir's Temporal worker: PaymentWorkflow and
// its activities, wired to the real Postgres repositories and the real
// Airwallex rail.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"enquoir/internal/adapters/airwallex"
	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/chaos"
	enquoirworkflow "enquoir/internal/workflow"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	chaos.WarnIfArmed(logger)
	ctx := context.Background()

	databaseURL := requireEnv(logger, "DATABASE_URL")
	awxBaseURL := requireEnv(logger, "AWX_BASE_URL")
	awxClientID := requireEnv(logger, "AWX_CLIENT_ID")
	awxAPIKey := requireEnv(logger, "AWX_API_KEY")
	temporalAddress := os.Getenv("TEMPORAL_ADDRESS")
	if temporalAddress == "" {
		temporalAddress = "127.0.0.1:7233"
	}

	pool, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		logger.Error("connecting to Postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	rail, err := airwallex.NewClient(awxBaseURL, awxClientID, awxAPIKey, &http.Client{Timeout: 30 * time.Second}, clock.System{})
	if err != nil {
		logger.Error("building Airwallex client", "error", err)
		os.Exit(1)
	}

	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		logger.Error("connecting to Temporal", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	activities := &enquoirworkflow.Activities{
		Intents: postgres.NewIntentRepository(pool),
		Events:  postgres.NewEventLog(pool),
		Rail:    rail,
	}

	w := worker.New(temporalClient, enquoirworkflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(enquoirworkflow.PaymentWorkflow)
	w.RegisterActivity(activities)

	logger.Info("worker starting", "task_queue", enquoirworkflow.TaskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
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
