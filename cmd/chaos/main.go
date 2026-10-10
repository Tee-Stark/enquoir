// Command chaos is the kill test: for each ENQUOIR_CHAOS_KILL_AT
// point, it submits one intent three times, lets a worker armed with
// that kill point die mid-payment, restarts a clean one, waits for the
// workflow to settle, and asserts the sandbox never ended up holding
// more than the one transfer, the intent never got stuck in UNKNOWN,
// and its hash chain still verifies.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"go.temporal.io/sdk/client"

	"enquoir/internal/adapters/airwallex"
	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/app"
	"enquoir/internal/chaos"
	"enquoir/internal/domain"
	enquoirworkflow "enquoir/internal/workflow"
)

const (
	demoAmountMinorUnits = 1000 // $10.00
	maxMinorUnits        = 1_000_000_00
	findTimeout          = 30 * time.Second
	settleTimeout        = 60 * time.Second
)

var points = []chaos.Point{
	chaos.BeforeLog,
	chaos.AfterLogBeforeCall,
	chaos.DuringCall,
	chaos.AfterCallBeforeCommit,
}

type pointResult struct {
	Point      chaos.Point
	Passed     bool
	FinalState domain.State
	TransferID string
	Detail     string
}

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

	httpClient := &http.Client{Timeout: 30 * time.Second}

	pool, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		logger.Error("connecting to Postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
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

	beneficiaryID, err := simulator.EnsureBeneficiary(ctx)
	if err != nil {
		logger.Error("ensuring test beneficiary", "error", err)
		os.Exit(1)
	}

	results := make([]pointResult, 0, len(points))
	for _, point := range points {
		logger.Info("chaos: running kill point", "point", point)
		results = append(results, runKillPoint(ctx, logger, temporalClient, rail, simulator, events, beneficiaryID, point))
	}

	printTable(results)

	for _, r := range results {
		if !r.Passed {
			os.Exit(1)
		}
	}
}

func runKillPoint(
	ctx context.Context,
	logger *slog.Logger,
	temporalClient client.Client,
	rail app.Rail,
	simulator *airwallex.Simulator,
	events app.EventLog,
	beneficiaryID string,
	point chaos.Point,
) pointResult {
	reference := fmt.Sprintf("chaos-%s-%d", point, time.Now().UnixNano())
	intent, err := domain.NewIntent(beneficiaryID, demoAmountMinorUnits, "USD", reference, "chaos test: "+string(point))
	if err != nil {
		return pointResult{Point: point, Detail: fmt.Sprintf("building intent: %v", err)}
	}

	opts := client.StartWorkflowOptions{ID: string(intent.Key), TaskQueue: enquoirworkflow.TaskQueue}
	input := enquoirworkflow.PaymentInput{Intent: intent, MaxAmountMinorUnits: maxMinorUnits}

	// Submit the same intent three times, same as an agent retrying.
	for i := 0; i < 3; i++ {
		if _, err := temporalClient.ExecuteWorkflow(ctx, opts, enquoirworkflow.PaymentWorkflow, input); err != nil {
			return pointResult{Point: point, Detail: fmt.Sprintf("submit %d: %v", i, err)}
		}
	}

	logger.Info("chaos: starting armed worker", "point", point)
	armed := exec.Command("go", "run", "./cmd/worker")
	armed.Env = append(os.Environ(), fmt.Sprintf("%s=%s", chaos.EnvVar, point))
	armed.Stdout = os.Stdout
	armed.Stderr = os.Stderr
	runErr := armed.Run()
	if _, ok := runErr.(*exec.ExitError); runErr != nil && !ok {
		return pointResult{Point: point, Detail: fmt.Sprintf("armed worker failed to start: %v", runErr)}
	}
	logger.Info("chaos: armed worker died as expected", "point", point)

	logger.Info("chaos: starting clean worker", "point", point)
	clean := exec.Command("go", "run", "./cmd/worker")
	clean.Env = os.Environ()
	clean.Stdout = os.Stdout
	clean.Stderr = os.Stderr
	if err := clean.Start(); err != nil {
		return pointResult{Point: point, Detail: fmt.Sprintf("clean worker failed to start: %v", err)}
	}
	defer func() {
		if clean.Process != nil {
			clean.Process.Kill()
		}
		clean.Wait()
	}()

	transfer, err := awaitTransfer(ctx, rail, string(intent.Key))
	if err != nil {
		return pointResult{Point: point, Detail: fmt.Sprintf("waiting for the transfer to exist: %v", err)}
	}
	if err := simulator.TransitionTransfer(ctx, transfer.ID, app.TransferSent); err != nil {
		logger.Warn("transitioning to SENT", "point", point, "error", err)
	}
	if err := simulator.TransitionTransfer(ctx, transfer.ID, app.TransferPaid); err != nil {
		logger.Warn("transitioning to PAID", "point", point, "error", err)
	}

	resultCtx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	handle := temporalClient.GetWorkflow(resultCtx, string(intent.Key), "")
	var wfResult enquoirworkflow.PaymentResult
	if err := handle.Get(resultCtx, &wfResult); err != nil {
		return pointResult{Point: point, Detail: fmt.Sprintf("waiting for workflow to settle: %v", err)}
	}

	chain, err := events.Load(ctx, intent.Key)
	if err != nil {
		return pointResult{Point: point, FinalState: wfResult.State, Detail: fmt.Sprintf("loading event chain: %v", err)}
	}
	chainErr := chain.Verify()

	_, found, findErr := rail.FindTransferByRequestID(ctx, string(intent.Key))

	passed := wfResult.State != domain.UNKNOWN && chainErr == nil && findErr == nil && found
	detail := "ok"
	if !passed {
		detail = fmt.Sprintf("chain_verify_err=%v find_err=%v found=%v", chainErr, findErr, found)
	}

	return pointResult{
		Point:      point,
		Passed:     passed,
		FinalState: wfResult.State,
		TransferID: wfResult.TransferID,
		Detail:     detail,
	}
}

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

func printTable(results []pointResult) {
	fmt.Println()
	fmt.Printf("%-24s %-6s %-10s %-24s %s\n", "KILL POINT", "PASS", "STATE", "TRANSFER ID", "DETAIL")
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("%-24s %-6s %-10s %-24s %s\n", r.Point, status, r.FinalState, r.TransferID, r.Detail)
	}
	fmt.Println()
}

func requireEnv(logger *slog.Logger, name string) string {
	v := os.Getenv(name)
	if v == "" {
		logger.Error("missing required environment variable", "name", name)
		os.Exit(1)
	}
	return v
}
