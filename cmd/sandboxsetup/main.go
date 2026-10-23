// Command sandboxsetup ensures the sandbox has what Enquoir's demo
// needs: a funded USD Global Account and one US/USD/LOCAL test
// beneficiary. Safe to run repeatedly - the account and beneficiary are
// looked up before being created, so re-running never duplicates them.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"enquoir/internal/adapters/airwallex"
	"enquoir/internal/adapters/clock"
	"enquoir/internal/domain"
	"enquoir/internal/sandbox"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	baseURL := os.Getenv("AWX_BASE_URL")
	clientID := os.Getenv("AWX_CLIENT_ID")
	apiKey := os.Getenv("AWX_API_KEY")
	if clientID == "" || apiKey == "" {
		logger.Error("AWX_CLIENT_ID and AWX_API_KEY must both be set")
		os.Exit(1)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	rail, err := airwallex.NewClient(baseURL, clientID, apiKey, httpClient, clock.System{})
	if err != nil {
		logger.Error("building Airwallex client", "error", err)
		os.Exit(1)
	}
	simulator, err := airwallex.NewSimulator(baseURL, clientID, apiKey, httpClient, clock.System{})
	if err != nil {
		logger.Error("building simulator", "error", err)
		os.Exit(1)
	}

	beneficiaryID, err := sandbox.EnsurePayee(ctx, rail)
	if err != nil {
		logger.Error("ensuring test beneficiary", "error", err)
		os.Exit(1)
	}
	logger.Info("test beneficiary ready", "beneficiary_id", beneficiaryID)

	fundAmount, err := domain.NewMoney(100000, domain.USD)
	if err != nil {
		logger.Error("building fund amount", "error", err)
		os.Exit(1)
	}
	if err := simulator.FundAccount(ctx, fundAmount); err != nil {
		logger.Error("funding account", "error", err)
		os.Exit(1)
	}
	logger.Info("USD global account funded", "amount", fundAmount.MajorUnitsString())
}
