// Command worker runs Enquoir's Temporal worker: PaymentWorkflow,
// BeneficiaryWorkflow and their activities, wired to the real Postgres
// repositories and the real Airwallex rail.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"enquoir/internal/adapters/airwallex"
	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/credentials"
	"enquoir/internal/adapters/emailtmpl"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/app"
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

	// Unarmed, ArmTransport is the default transport unchanged. Armed at
	// during_call, it kills this process once a transfer create has
	// fully left it, before any response is read.
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: chaos.ArmTransport(http.DefaultTransport.(*http.Transport), chaos.DuringCall, isTransferCreate),
	}
	rail, err := airwallex.NewClient(awxBaseURL, awxClientID, awxAPIKey, httpClient, clock.System{})
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

	mailer, mode, err := mailerFromEnv(os.Getenv)
	if err != nil {
		logger.Error("configuring email", "error", err)
		os.Exit(1)
	}
	if mode == "file" {
		logger.Warn("EMAIL_PROVIDER=file: emails are written to MAIL_FILE, not sent; the links in it are live credentials - development only",
			"mail_file", os.Getenv("MAIL_FILE"))
	}
	appBaseURL := os.Getenv("APP_BASE_URL")
	if appBaseURL == "" {
		appBaseURL = "https://enquoir.etimi.me"
	}
	outbox := postgres.NewEmailOutbox(pool)

	activities := &enquoirworkflow.Activities{
		Intents:       postgres.NewIntentRepository(pool),
		Events:        postgres.NewEventLog(pool),
		Rail:          rail,
		Beneficiaries: rail,
		Attempts:      postgres.NewAttemptLog(pool),
		IDs:           credentials.RandomIDs{},
		Clock:         clock.System{},

		EmailDelivery: &app.DeliverEmail{
			Outbox: outbox, Tokens: postgres.NewTokenStore(pool), Renderer: emailtmpl.New(), Mailer: mailer,
			Secrets: credentials.RandomSecrets{}, Clock: clock.System{}, AppBaseURL: appBaseURL,
		},
		EmailGiveUp: &app.GiveUpEmail{Outbox: outbox, Clock: clock.System{}},
	}

	stopSweep := make(chan struct{})
	defer close(stopSweep)
	go sweepQueuedEmails(logger, outbox, &enquoirworkflow.EmailDispatcher{Client: temporalClient}, stopSweep)

	w := worker.New(temporalClient, enquoirworkflow.TaskQueue, worker.Options{})
	w.RegisterWorkflow(enquoirworkflow.PaymentWorkflow)
	w.RegisterWorkflow(enquoirworkflow.BeneficiaryWorkflow)
	w.RegisterWorkflow(enquoirworkflow.SendEmailWorkflow)
	w.RegisterActivity(activities)

	logger.Info("worker starting", "task_queue", enquoirworkflow.TaskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

// sweepQueuedEmails re-dispatches emails left queued - their durable
// send never started (Temporal was unreachable when they were queued).
// Dispatching one already sending attaches, so this is always safe.
func sweepQueuedEmails(logger *slog.Logger, outbox *postgres.EmailOutbox, d *enquoirworkflow.EmailDispatcher, stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		ids, err := outbox.Queued(ctx, time.Now().Add(-2*time.Minute), 100)
		if err != nil {
			logger.Warn("sweeping queued emails", "error", err)
		}
		for _, id := range ids {
			if err := d.Dispatch(ctx, id); err != nil {
				logger.Warn("re-dispatching a queued email", "email", id, "error", err)
			}
		}
		cancel()
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

func isTransferCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/transfers/create")
}

func requireEnv(logger *slog.Logger, name string) string {
	v := os.Getenv(name)
	if v == "" {
		logger.Error("missing required environment variable", "name", name)
		os.Exit(1)
	}
	return v
}
