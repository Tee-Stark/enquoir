// Command api runs Enquoir's HTTP server.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/joho/godotenv"
	"go.temporal.io/sdk/client"

	"enquoir/internal/adapters/httpapi"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/app"
)

const defaultMaxMinorUnits = 1_000_000_00

func main() {
	if err := godotenv.Load(); err != nil {
		slog.Warn("No .env file found or failed to load")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	databaseURL := requireEnv(logger, "DATABASE_URL")
	temporalAddress := os.Getenv("TEMPORAL_ADDRESS")
	if temporalAddress == "" {
		temporalAddress = "127.0.0.1:7233"
	}
	httpAddr := os.Getenv("HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8090"
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

	pool, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		logger.Error("connecting to Postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	events := postgres.NewEventLog(pool)

	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		logger.Error("connecting to Temporal", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	handler := &httpapi.Handler{
		Submit: &app.SubmitIntent{
			Starter: &httpapi.TemporalPaymentStarter{Client: temporalClient, MaxAmountMinorUnits: maxMinorUnits},
			Events:  events,
		},
		Get:      &app.GetIntent{Events: events},
		Register: &app.RegisterBeneficiary{Registrar: &httpapi.TemporalBeneficiaryRegistrar{Client: temporalClient}},
	}

	logger.Info("api listening", "addr", httpAddr)
	if err := http.ListenAndServe(httpAddr, httpapi.NewRouter(handler)); err != nil {
		logger.Error("server stopped", "error", err)
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
