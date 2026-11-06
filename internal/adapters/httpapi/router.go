// Package httpapi is the HTTP adapter: chi handlers that parse a
// request, call a use case, and respond. No business logic lives here.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/domain"
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
	SwitchOrg      *app.SwitchOrganisation
	MyOrgs         *app.ListMyOrganisations
	VerifyEmail    *app.VerifyEmail
	ResendVerify   *app.ResendVerification
	ForgotPassword *app.ForgotPassword
	ResetPassword  *app.ResetPassword
	ChangePassword *app.ChangePassword
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
	r.With(h.requireUser).Post("/v1/auth/switch-organisation", h.switchOrganisation)

	r.Post("/v1/auth/verify-email", h.verifyEmail)
	r.With(h.requireUser).Post("/v1/auth/verify-email/resend", h.resendVerification)
	r.Post("/v1/auth/password/forgot", h.forgotPassword)
	r.Post("/v1/auth/password/reset", h.resetPassword)
	r.With(h.requireUser).Post("/v1/auth/password/change", h.changePassword)

	// Placeholders until their requirements arrive.
	r.With(h.requireUser).Get("/v1/onboarding", notImplemented("Onboarding"))
	r.With(h.requireUser).Put("/v1/onboarding", notImplemented("Onboarding"))

	// The dashboard. Every route names the permission it needs; the
	// table of who has which is domain.Role.
	r.Group(func(r chi.Router) {
		r.Use(h.requireUser)
		read, write := h.allow(domain.PermRead), h.allow(domain.PermWrite)
		verified := requireVerified

		r.With(write, verified).Post("/v1/agents", h.createAgent)
		r.With(read).Get("/v1/agents", h.listAgents)
		r.With(read).Get("/v1/agents/{id}", h.getAgent)
		r.With(write).Delete("/v1/agents/{id}", h.revokeAgent)
		r.With(write, verified).Post("/v1/agents/{id}/rotate-key", h.rotateAgentKey)

		r.With(read).Get("/v1/payments", h.listPayments)
		r.With(read).Get("/v1/payments/{key}", h.getPayment)
		r.With(read).Get("/v1/payments/{key}/replay", h.replayPayment)
		r.With(read).Get("/v1/attempts", h.listAttempts)
		r.With(read).Get("/v1/beneficiaries", h.listBeneficiaries)

		r.With(read).Get("/v1/analytics/summary", h.analyticsSummary)
		r.With(read).Get("/v1/analytics/timeseries", h.analyticsTimeSeries)
	})
	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
