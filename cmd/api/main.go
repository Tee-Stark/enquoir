// Command api runs Enquoir's HTTP server.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"go.temporal.io/sdk/client"

	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/credentials"
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

	sessionTTL := 7 * 24 * time.Hour
	if v := os.Getenv("SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			logger.Error("invalid SESSION_TTL", "value", v)
			os.Exit(1)
		}
		sessionTTL = d
	}
	secureCookies := os.Getenv("SESSION_COOKIE_SECURE") != "false"
	if !secureCookies {
		logger.Warn("session cookies are not Secure: only for plain-HTTP local development")
	}

	sysClock := clock.System{}
	accounts := postgres.NewAccountStore(pool)
	sessions := postgres.NewSessionStore(pool)
	hasher := &credentials.PBKDF2Hasher{}
	ids := credentials.RandomIDs{}
	agents := postgres.NewAgentStore(pool)
	beneficiaries := postgres.NewBeneficiaryStore(pool)
	secrets := credentials.RandomSecrets{}
	issuer := &app.SessionIssuer{Sessions: sessions, Secrets: secrets, Clock: sysClock, TTL: sessionTTL}

	handler := &httpapi.Handler{
		Submit: &app.SubmitIntent{
			Starter: &httpapi.TemporalPaymentStarter{Client: temporalClient, MaxAmountMinorUnits: maxMinorUnits},
			Events:  events,
			Payees:  beneficiaries,
			Intents: postgres.NewIntentRepository(pool),
		},
		Get: &app.GetIntent{Events: events},
		Register: &app.RegisterBeneficiary{
			Registrar:     &httpapi.TemporalBeneficiaryRegistrar{Client: temporalClient},
			Beneficiaries: beneficiaries,
			Clock:         sysClock,
		},

		SignUp:         &app.SignUp{Accounts: accounts, Hasher: hasher, IDs: ids, Clock: sysClock, Issuer: issuer},
		LogIn:          &app.LogIn{Accounts: accounts, Hasher: hasher, Issuer: issuer},
		LogOut:         &app.LogOut{Sessions: sessions, Clock: sysClock},
		ResolveSession: &app.ResolveSession{Sessions: sessions, Accounts: accounts, Clock: sysClock},
		Cookies:        httpapi.CookieSettings{Secure: secureCookies},

		CreateAgent:       &app.CreateAgent{Agents: agents, IDs: ids, Secrets: secrets, Clock: sysClock},
		ListAgents:        &app.ListAgents{Agents: agents},
		GetAgent:          &app.GetAgent{Agents: agents},
		RotateAgentKey:    &app.RotateAgentKey{Agents: agents, IDs: ids, Secrets: secrets, Clock: sysClock},
		RevokeAgent:       &app.RevokeAgent{Agents: agents, Clock: sysClock},
		AuthenticateAgent: &app.AuthenticateAgent{Agents: agents, Clock: sysClock},
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
