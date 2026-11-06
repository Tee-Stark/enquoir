// Command chaos is the kill test: for each ENQUOIR_CHAOS_KILL_AT
// point, it submits one intent three times, lets a worker armed with
// that kill point die mid-payment, restarts a clean one, waits for the
// workflow to settle, and asserts the sandbox never ended up holding
// more than the one transfer, the intent never got stuck in UNKNOWN,
// and its hash chain still verifies.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/client"

	"enquoir/internal/adapters/airwallex"
	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/credentials"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/app"
	"enquoir/internal/chaos"
	"enquoir/internal/domain"
	"enquoir/internal/sandbox"
	enquoirworkflow "enquoir/internal/workflow"
)

const (
	demoAmountMinorUnits = 1000 // $10.00
	maxMinorUnits        = 1_000_000_00
	// findTimeout must outlast the activity StartToCloseTimeout (30s):
	// after a kill before the create, Temporal only redispatches the
	// lost activity once that timeout expires, and only then is the
	// transfer created.
	findTimeout   = 90 * time.Second
	settleTimeout = 60 * time.Second
	// armedTimeout bounds an armed worker that never reaches its kill
	// point - otherwise the run would hang forever.
	armedTimeout = 2 * time.Minute
	// killLogLine is what chaos.KillAt logs right before os.Exit(1). An
	// armed worker only counts as killed at its point if this appears.
	killLogLine = "CHAOS: killing the process now"
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
	History    string
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

	org, err := sandbox.EnsureDemoOrg(ctx, postgres.NewAccountStore(pool), credentials.RandomIDs{}, clock.System{})
	if err != nil {
		logger.Error("ensuring demo organisation", "error", err)
		os.Exit(1)
	}

	beneficiaryID, err := sandbox.EnsurePayee(ctx, rail, postgres.NewBeneficiaryStore(pool), clock.System{}, org)
	if err != nil {
		logger.Error("ensuring test beneficiary", "error", err)
		os.Exit(1)
	}

	// Build the worker once and run the binary directly. Killing a
	// `go run` process does not kill the program it compiled and
	// started: the real worker would be orphaned, keep polling the
	// task queue, and steal the next kill point's work from its armed
	// worker.
	workerBin, cleanup, err := buildWorker()
	if err != nil {
		logger.Error("building the worker", "error", err)
		os.Exit(1)
	}
	defer cleanup()

	results := make([]pointResult, 0, len(points))
	for _, point := range points {
		logger.Info("chaos: running kill point", "point", point)
		results = append(results, runKillPoint(ctx, logger, temporalClient, rail, simulator, events, workerBin, org, beneficiaryID, point))
	}

	printTable(results)

	for _, r := range results {
		if !r.Passed {
			cleanup()
			os.Exit(1)
		}
	}
}

func buildWorker() (bin string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "enquoir-chaos-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	bin = filepath.Join(dir, "worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/worker")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		cleanup()
		return "", nil, err
	}
	return bin, cleanup, nil
}

// lockedBuffer collects a subprocess's output, which arrives on two
// pipes written from separate goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runArmed runs a worker armed to die at point and reports an error
// unless it died there: exit code 1 with the kill switch's own log
// line. Any other exit - a startup failure, a crash elsewhere, or
// never reaching the point before armedTimeout - would otherwise let
// a point pass without anything having been killed.
func runArmed(ctx context.Context, workerBin string, point chaos.Point) error {
	ctx, cancel := context.WithTimeout(ctx, armedTimeout)
	defer cancel()

	var out lockedBuffer
	armed := exec.CommandContext(ctx, workerBin)
	armed.Env = append(os.Environ(), fmt.Sprintf("%s=%s", chaos.EnvVar, point))
	armed.Stdout = io.MultiWriter(os.Stdout, &out)
	armed.Stderr = io.MultiWriter(os.Stderr, &out)
	err := armed.Run()

	if ctx.Err() != nil {
		return fmt.Errorf("armed worker never reached %s within %s", point, armedTimeout)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("armed worker did not exit with an error: %v", err)
	}
	if exitErr.ExitCode() != 1 {
		return fmt.Errorf("armed worker exited %d, want 1", exitErr.ExitCode())
	}
	if !strings.Contains(out.String(), killLogLine) {
		return fmt.Errorf("armed worker exited 1 without logging %q: it died somewhere other than %s", killLogLine, point)
	}
	return nil
}

// driveTo moves a transfer to status through the simulator and
// confirms it by reading the transfer back. The sandbox has refused a
// transition sent straight after the previous one, so each is retried
// until the rail reports the new status.
func driveTo(ctx context.Context, rail app.Rail, simulator *airwallex.Simulator, transferID string, status app.TransferStatus) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		if t, err := rail.GetTransfer(ctx, transferID); err == nil && t.Status == status {
			return nil
		}
		if err := simulator.TransitionTransfer(ctx, transferID, status); err != nil {
			lastErr = err
			continue
		}
		t, err := rail.GetTransfer(ctx, transferID)
		if err == nil && t.Status == status {
			return nil
		}
		lastErr = fmt.Errorf("transition accepted but transfer reads %q (err %v)", t.Status, err)
	}
	return fmt.Errorf("transfer %s never reached %s: %w", transferID, status, lastErr)
}

func runKillPoint(
	ctx context.Context,
	logger *slog.Logger,
	temporalClient client.Client,
	rail app.Rail,
	simulator *airwallex.Simulator,
	events app.EventLog,
	workerBin string,
	org domain.OrgID,
	beneficiaryID string,
	point chaos.Point,
) pointResult {
	reference := fmt.Sprintf("chaos-%s-%d", point, time.Now().UnixNano())
	intent, err := domain.NewIntent(org, beneficiaryID, demoAmountMinorUnits, "USD", reference, "chaos test: "+string(point))
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
	if err := runArmed(ctx, workerBin, point); err != nil {
		return pointResult{Point: point, Detail: err.Error()}
	}
	logger.Info("chaos: armed worker died at its kill point", "point", point)

	logger.Info("chaos: starting clean worker", "point", point)
	clean := exec.Command(workerBin)
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
	for _, status := range []app.TransferStatus{app.TransferSent, app.TransferPaid} {
		if err := driveTo(ctx, rail, simulator, transfer.ID, status); err != nil {
			return pointResult{Point: point, TransferID: transfer.ID, Detail: err.Error()}
		}
	}

	resultCtx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	handle := temporalClient.GetWorkflow(resultCtx, string(intent.Key), "")
	var wfResult enquoirworkflow.PaymentResult
	if err := handle.Get(resultCtx, &wfResult); err != nil {
		return pointResult{Point: point, Detail: fmt.Sprintf("waiting for workflow to settle: %v", err)}
	}

	chain, err := events.Load(ctx, intent.OrgID, intent.Key)
	if err != nil {
		return pointResult{Point: point, FinalState: wfResult.State, Detail: fmt.Sprintf("loading event chain: %v", err)}
	}
	chainErr := chain.Verify()
	types := make([]string, len(chain))
	for i, e := range chain {
		types[i] = e.Type
	}
	history := strings.Join(types, ">")

	// FindTransferByRequestID matches request_id exactly and returns
	// ErrDuplicateTransfers on more than one match, so found with no
	// error means exactly one transfer for this intent on the rail.
	onRail, found, findErr := rail.FindTransferByRequestID(ctx, string(intent.Key))

	passed := wfResult.State == domain.APPLIED &&
		chainErr == nil && findErr == nil && found &&
		onRail.ID == wfResult.TransferID
	detail := "ok"
	if !passed {
		detail = fmt.Sprintf("state=%s chain_verify_err=%v find_err=%v found=%v rail_id=%s",
			wfResult.State, chainErr, findErr, found, onRail.ID)
	}

	return pointResult{
		Point:      point,
		Passed:     passed,
		FinalState: wfResult.State,
		TransferID: wfResult.TransferID,
		History:    history,
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
	fmt.Printf("%-24s %-6s %-10s %-36s %-60s %s\n", "KILL POINT", "PASS", "STATE", "TRANSFER ID", "HISTORY", "DETAIL")
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("%-24s %-6s %-10s %-36s %-60s %s\n", r.Point, status, r.FinalState, r.TransferID, r.History, r.Detail)
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
