// Command api runs Enquoir's HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	// Analytics resolve any IANA time zone, even in an image with no
	// zone database of its own.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"go.temporal.io/sdk/client"

	"enquoir/internal/adapters/clock"
	"enquoir/internal/adapters/credentials"
	"enquoir/internal/adapters/httpapi"
	"enquoir/internal/adapters/postgres"
	"enquoir/internal/adapters/redisstore"
	"enquoir/internal/app"
	"enquoir/internal/commerce/commerceapp"
	enquoirworkflow "enquoir/internal/workflow"
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
	attempts := postgres.NewAttemptLog(pool)
	payments := postgres.NewPaymentReader(pool)
	analytics := postgres.NewAnalyticsReader(pool)
	secrets := credentials.RandomSecrets{}
	throttle, cache := connectRedis(logger)
	auditLog := postgres.NewAuditLog(pool)
	audit := &app.Audit{Log: auditLog, IDs: ids, Clock: sysClock}

	emails := &app.Emails{Outbox: postgres.NewEmailOutbox(pool), Dispatcher: &enquoirworkflow.EmailDispatcher{Client: temporalClient}, IDs: ids, Clock: sysClock}
	tokens := postgres.NewTokenStore(pool)
	invitations := postgres.NewInvitationStore(pool)
	issuer := &app.SessionIssuer{Sessions: sessions, IDs: ids, Secrets: secrets, Clock: sysClock, TTL: sessionTTL}

	handler := &httpapi.Handler{
		Submit: &app.SubmitIntent{
			Starter:  &httpapi.TemporalPaymentStarter{Client: temporalClient, MaxAmountMinorUnits: maxMinorUnits},
			Events:   events,
			Payees:   beneficiaries,
			Intents:  postgres.NewIntentRepository(pool),
			Attempts: attempts,
			IDs:      ids,
			Clock:    sysClock,
		},
		Get: &app.GetIntent{Events: events},
		Register: &app.RegisterBeneficiary{
			Registrar:     &httpapi.TemporalBeneficiaryRegistrar{Client: temporalClient},
			Beneficiaries: beneficiaries,
			Clock:         sysClock,
			Audit:         audit,
		},

		SignUp:         &app.SignUp{Accounts: accounts, Hasher: hasher, IDs: ids, Clock: sysClock, Issuer: issuer, Emails: emails},
		VerifyEmail:    &app.VerifyEmail{Tokens: tokens, Users: accounts, Clock: sysClock, Cache: cache, Audit: audit},
		ResendVerify:   &app.ResendVerification{Emails: emails, Throttle: throttle},
		ForgotPassword: &app.ForgotPassword{Accounts: accounts, Emails: emails, Throttle: throttle},
		ResetPassword:  &app.ResetPassword{Tokens: tokens, Users: accounts, Accounts: accounts, Sessions: sessions, Hasher: hasher, Emails: emails, Clock: sysClock, Cache: cache, Throttle: throttle, Audit: audit},
		ChangePassword: &app.ChangePassword{Users: accounts, Accounts: accounts, Sessions: sessions, Hasher: hasher, Emails: emails, Clock: sysClock, Cache: cache, Audit: audit, Throttle: throttle},

		ListSessions:        &app.ListSessions{Sessions: sessions, Clock: sysClock},
		RevokeSession:       &app.RevokeSession{Sessions: sessions, Clock: sysClock, Cache: cache},
		RevokeOtherSessions: &app.RevokeOtherSessions{Sessions: sessions, Clock: sysClock, Cache: cache, Audit: audit},

		ListAuditLog:   &app.ListAuditLog{Log: auditLog},
		ListMyActivity: &app.ListMyActivity{Log: auditLog},

		ListMembers:      &app.ListMembers{Members: accounts},
		ChangeMemberRole: &app.ChangeMemberRole{Members: accounts, Sessions: sessions, Clock: sysClock, Cache: cache, Audit: audit},
		RemoveMember:     &app.RemoveMember{Members: accounts, Sessions: sessions, Clock: sysClock, Cache: cache, Audit: audit},
		InviteMember:     &app.InviteMember{Accounts: accounts, Members: accounts, Invitations: invitations, Emails: emails, IDs: ids, Clock: sysClock, Audit: audit},
		ListInvitations:  &app.ListInvitations{Invitations: invitations, Clock: sysClock},
		RevokeInvitation: &app.RevokeInvitation{Invitations: invitations, Clock: sysClock, Audit: audit},
		AcceptInvitation: &app.AcceptInvitation{Tokens: tokens, Invitations: invitations, Accounts: accounts, Sessions: sessions, Hasher: hasher, IDs: ids, Issuer: issuer, Clock: sysClock, Cache: cache, Audit: audit},
		LogIn:            &app.LogIn{Accounts: accounts, Hasher: hasher, Issuer: issuer, Throttle: throttle, Audit: audit},
		LogOut:           &app.LogOut{Sessions: sessions, Clock: sysClock, Cache: cache},
		ResolveSession:   &app.ResolveSession{Sessions: sessions, Accounts: accounts, Clock: sysClock, Cache: cache},
		SwitchOrg:        &app.SwitchOrganisation{Accounts: accounts, Sessions: sessions, Issuer: issuer, Cache: cache},
		MyOrgs:           &app.ListMyOrganisations{Accounts: accounts},
		Cookies:          httpapi.CookieSettings{Secure: secureCookies},
		Origins:          httpapi.ParseAllowedOrigins(os.Getenv("CORS_ALLOWED_ORIGINS")),
		Throttle:         throttle,
		ClientIPHeader:   os.Getenv("CLIENT_IP_HEADER"),

		CreateAgent:       &app.CreateAgent{Agents: agents, IDs: ids, Secrets: secrets, Clock: sysClock, Audit: audit},
		ListAgents:        &app.ListAgents{Agents: agents},
		GetAgent:          &app.GetAgent{Agents: agents},
		RotateAgentKey:    &app.RotateAgentKey{Agents: agents, IDs: ids, Secrets: secrets, Clock: sysClock, Cache: cache, Audit: audit},
		RevokeAgent:       &app.RevokeAgent{Agents: agents, Clock: sysClock, Cache: cache, Audit: audit},
		AuthenticateAgent: &app.AuthenticateAgent{Agents: agents, Clock: sysClock, Cache: cache},

		ListPayments:      &app.ListPayments{Payments: payments},
		GetPayment:        &app.GetPayment{Payments: payments, Events: events, Attempts: attempts},
		ListAttempts:      &app.ListAttempts{Attempts: attempts},
		ListBeneficiaries: &app.ListBeneficiaries{Beneficiaries: beneficiaries},
		Summary:           &app.GetSummary{Analytics: analytics, Clock: sysClock},
		TimeSeries:        &app.GetTimeSeries{Analytics: analytics, Clock: sysClock},
		ReplayPayment:     &app.ReplayPayment{Get: &app.GetPayment{Payments: payments, Events: events, Attempts: attempts}},

		Commerce: commerceUseCases(pool, ids, secrets, sysClock, cache, audit),
		Funding:  fundingUseCases(pool, ids, sysClock, audit),
		Mandates: mandateUseCases(pool, tokens, emails, ids, sysClock, audit),
	}

	srv := httpapi.NewServer(httpAddr, httpapi.NewRouter(handler))
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-stop.Done()
		// Let requests in flight finish; refuse new ones.
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			logger.Error("shutting down", "error", err)
		}
	}()

	logger.Info("api listening", "addr", httpAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
	logger.Info("api stopped")
}

func requireEnv(logger *slog.Logger, name string) string {
	v := os.Getenv(name)
	if v == "" {
		logger.Error("missing required environment variable", "name", name)
		os.Exit(1)
	}
	return v
}

// connectRedis returns the rate limiter and the principal cache. With
// no REDIS_URL, or a Redis that does not answer at start-up, limits
// are kept per process and nothing is cached: authentication works
// either way.
func connectRedis(logger *slog.Logger) (*app.Throttle, *app.Cache) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		logger.Warn("REDIS_URL not set: rate limits are per process, and sessions and keys are not cached")
		return &app.Throttle{Counter: redisstore.NewMemoryCounter()}, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		logger.Error("invalid REDIS_URL", "error", err)
		os.Exit(1)
	}
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout = time.Second, 250*time.Millisecond, 250*time.Millisecond
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Warn("redis unreachable at start-up: rate limits are per process, and sessions and keys are not cached", "error", err)
		return &app.Throttle{Counter: redisstore.NewMemoryCounter()}, nil
	}
	logger.Info("redis connected: shared rate limits and principal cache")
	throttle := &app.Throttle{
		Counter: redisstore.NewCounter(rdb),
		// During a Redis outage, limits and the lockout hold per process.
		Fallback: redisstore.NewMemoryCounter(),
		OnError: everyMinute(func(err error) {
			logger.Warn("redis rate limiting failed: using per-process limits until it is back", "error", err)
		}),
	}
	return throttle, &app.Cache{Store: redisstore.NewPrincipalStore(rdb)}
}

// everyMinute calls report at most once a minute, so an outage is
// visible in the logs without flooding them.
func everyMinute(report func(error)) func(error) {
	var mu sync.Mutex
	var last time.Time
	return func(err error) {
		mu.Lock()
		due := time.Since(last) >= time.Minute
		if due {
			last = time.Now()
		}
		mu.Unlock()
		if due {
			report(err)
		}
	}
}

// commerceUseCases wires merchant-delegated commerce: the dashboard's
// merchant integrations and the merchant's /v1/commerce routes.
func commerceUseCases(pool *pgxpool.Pool, ids app.IDGenerator, secrets app.SecretGenerator, clock app.Clock, cache *app.Cache, audit *app.Audit) httpapi.CommerceUseCases {
	integrations := postgres.NewMerchantIntegrationStore(pool)
	customers := postgres.NewCommerceCustomerStore(pool)
	agents := postgres.NewCommerceAgentStore(pool)
	return httpapi.CommerceUseCases{
		CreateIntegration:    &commerceapp.CreateIntegration{Integrations: integrations, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		ListIntegrations:     &commerceapp.ListIntegrations{Integrations: integrations},
		GetIntegration:       &commerceapp.GetIntegration{Integrations: integrations},
		SetIntegrationStatus: &commerceapp.SetIntegrationStatus{Integrations: integrations, Clock: clock, Cache: cache, Audit: audit},
		RotateIntegrationKey: &commerceapp.RotateIntegrationKey{Integrations: integrations, IDs: ids, Secrets: secrets, Clock: clock, Cache: cache, Audit: audit},
		RevokeIntegrationKey: &commerceapp.RevokeIntegrationKey{Integrations: integrations, Clock: clock, Cache: cache, Audit: audit},
		AuthenticateMerchant: &commerceapp.AuthenticateMerchant{Integrations: integrations, Clock: clock, Cache: cache},

		RegisterCustomer:  &commerceapp.RegisterCustomer{Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		ListCustomers:     &commerceapp.ListCustomers{Customers: customers},
		GetCustomer:       &commerceapp.GetCustomer{Customers: customers},
		SetCustomerStatus: &commerceapp.SetCustomerStatus{Customers: customers, Clock: clock, Audit: audit},

		RegisterAgent:  &commerceapp.RegisterAgent{Agents: agents, IDs: ids, Clock: clock, Audit: audit},
		ListAgents:     &commerceapp.ListAgents{Agents: agents},
		GetAgent:       &commerceapp.GetAgent{Agents: agents},
		SetAgentStatus: &commerceapp.SetAgentStatus{Agents: agents, Clock: clock, Audit: audit},
	}
}

// fundingUseCases wires funding-method bindings on /v1/commerce.
func fundingUseCases(pool *pgxpool.Pool, ids app.IDGenerator, clock app.Clock, audit *app.Audit) httpapi.FundingUseCases {
	methods, customers := postgres.NewCommerceFundingMethodStore(pool), postgres.NewCommerceCustomerStore(pool)
	return httpapi.FundingUseCases{
		Register: &commerceapp.RegisterFundingMethod{Methods: methods, Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		Get:      &commerceapp.GetFundingMethod{Methods: methods},
		List:     &commerceapp.ListFundingMethods{Methods: methods, Customers: customers},
		Status:   &commerceapp.SetFundingMethodStatus{Methods: methods, Clock: clock, Audit: audit},
	}
}

// mandateUseCases wires mandate requests and mandates on /v1/commerce,
// and the customer's consent page routes.
func mandateUseCases(pool *pgxpool.Pool, tokens app.TokenStore, emails *app.Emails, ids app.IDGenerator, clock app.Clock, audit *app.Audit) httpapi.MandateUseCases {
	store := postgres.NewCommerceMandateStore(pool)
	customers, agents, methods := postgres.NewCommerceCustomerStore(pool), postgres.NewCommerceAgentStore(pool), postgres.NewCommerceFundingMethodStore(pool)
	return httpapi.MandateUseCases{
		Request:    &commerceapp.RequestMandate{Mandates: store, Customers: customers, Agents: agents, Methods: methods, Emails: emails, IDs: ids, Clock: clock, Audit: audit},
		GetRequest: &commerceapp.GetMandateRequest{Mandates: store, Clock: clock},
		View:       &commerceapp.ViewConsent{Tokens: tokens, Mandates: store, Clock: clock},
		Decide:     &commerceapp.DecideConsent{Tokens: tokens, Mandates: store, IDs: ids, Clock: clock, Audit: audit},
		List:       &commerceapp.ListMandates{Mandates: store, Clock: clock},
		Get:        &commerceapp.GetMandate{Mandates: store, Clock: clock},
		Revoke:     &commerceapp.RevokeMandate{Mandates: store, Customers: customers, Emails: emails, Clock: clock, Audit: audit},
	}
}
