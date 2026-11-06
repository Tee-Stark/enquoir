// Package httpapi is the HTTP adapter: chi handlers that parse a
// request, call a use case, and respond. No business logic lives here.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
)

// Handler holds the use cases the API calls into.
type Handler struct {
	Submit   *app.SubmitIntent
	Get      *app.GetIntent
	Register *app.RegisterBeneficiary

	SignUp         *app.SignUp
	LogIn          *app.LogIn
	LogOut         *app.LogOut
	ResolveSession *app.ResolveSession
	Cookies        CookieSettings
	Origins        AllowedOrigins

	CreateAgent       *app.CreateAgent
	ListAgents        *app.ListAgents
	GetAgent          *app.GetAgent
	RotateAgentKey    *app.RotateAgentKey
	RevokeAgent       *app.RevokeAgent
	AuthenticateAgent *app.AuthenticateAgent

	ListPayments      *app.ListPayments
	GetPayment        *app.GetPayment
	ListAttempts      *app.ListAttempts
	ListBeneficiaries *app.ListBeneficiaries
	ReplayPayment     *app.ReplayPayment
	Summary           *app.GetSummary
	TimeSeries        *app.GetTimeSeries
}

func NewRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Use(limitBody, h.cors, h.checkOrigin)
	r.Get("/healthz", h.healthz)
	r.Get("/openapi.yaml", serveSpec)
	r.Get("/docs", serveDocs)

	r.Group(func(r chi.Router) {
		r.Use(h.requireAgent)
		r.Post("/v1/intents", h.submitIntent)
		r.Get("/v1/intents/{key}", h.getIntent)
		r.Post("/v1/beneficiaries", h.registerBeneficiary)
	})

	r.Post("/v1/auth/signup", h.signUp)
	r.Post("/v1/auth/login", h.logIn)
	r.Post("/v1/auth/logout", h.logOut)
	r.With(h.requireUser).Get("/v1/auth/session", h.getSession)

	// Placeholders until their requirements arrive.
	r.Post("/v1/auth/verify-email", notImplemented("Email verification"))
	r.With(h.requireUser).Post("/v1/auth/verify-email/resend", notImplemented("Email verification"))
	r.With(h.requireUser).Get("/v1/onboarding", notImplemented("Onboarding"))
	r.With(h.requireUser).Put("/v1/onboarding", notImplemented("Onboarding"))

	r.Group(func(r chi.Router) {
		r.Use(h.requireUser)
		r.Post("/v1/agents", h.createAgent)
		r.Get("/v1/agents", h.listAgents)
		r.Get("/v1/agents/{id}", h.getAgent)
		r.Delete("/v1/agents/{id}", h.revokeAgent)
		r.Post("/v1/agents/{id}/rotate-key", h.rotateAgentKey)

		r.Get("/v1/payments", h.listPayments)
		r.Get("/v1/payments/{key}", h.getPayment)
		r.Get("/v1/payments/{key}/replay", h.replayPayment)
		r.Get("/v1/attempts", h.listAttempts)
		r.Get("/v1/beneficiaries", h.listBeneficiaries)

		r.Get("/v1/analytics/summary", h.analyticsSummary)
		r.Get("/v1/analytics/timeseries", h.analyticsTimeSeries)
	})
	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
