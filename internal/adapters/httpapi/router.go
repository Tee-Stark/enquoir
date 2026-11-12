// Package httpapi is the HTTP adapter: chi handlers that parse a
// request, call a use case, and respond. No business logic lives here.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
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

	ListSessions        *app.ListSessions
	RevokeSession       *app.RevokeSession
	RevokeOtherSessions *app.RevokeOtherSessions

	ListAuditLog   *app.ListAuditLog
	ListMyActivity *app.ListMyActivity

	ListMembers      *app.ListMembers
	ChangeMemberRole *app.ChangeMemberRole
	RemoveMember     *app.RemoveMember
	InviteMember     *app.InviteMember
	ListInvitations  *app.ListInvitations
	RevokeInvitation *app.RevokeInvitation
	AcceptInvitation *app.AcceptInvitation
	Cookies          CookieSettings
	Origins          AllowedOrigins
	Throttle         *app.Throttle
	ClientIPHeader   string // the proxy header carrying the client IP, if any

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

	Commerce   CommerceUseCases
	Funding    FundingUseCases
	Mandates   MandateUseCases
	Checkouts  CheckoutUseCases
	Grants     GrantUseCases
	Executions ExecutionUseCases
}

func NewRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Use(limitBody, h.cors, h.checkOrigin, h.withActor)
	r.Get("/healthz", h.healthz)
	r.Get("/openapi.yaml", serveSpec)
	r.Get("/docs", serveDocs)

	r.Group(func(r chi.Router) {
		r.Use(h.requireAgent)
		r.Post("/v1/intents", h.submitIntent)
		r.Get("/v1/intents/{key}", h.getIntent)
	})
	// Agents register payees with their key; members from the
	// dashboard, as operator or above with a verified email.
	r.With(h.agentOrMember(domain.PermWrite)).Post("/v1/beneficiaries", h.registerBeneficiary)

	// Merchant-delegated commerce: the merchant's backend, with its
	// integration key. The merchant is always the key's.
	r.Route("/v1/commerce", func(r chi.Router) {
		r.Use(h.requireMerchant)
		r.With(h.limitMerchant(app.RuleCommerceWritePerMerchant)).Post("/customers", h.registerCustomer)
		r.Get("/customers", h.listCustomers)
		r.Get("/customers/{id}", h.getCustomer)
		r.Post("/customers/{id}/disable", h.setCustomerStatus(commerce.Disabled))
		r.Post("/customers/{id}/enable", h.setCustomerStatus(commerce.Active))
		r.With(h.limitMerchant(app.RuleCommerceWritePerMerchant)).Post("/agents", h.registerCommerceAgent)
		r.Get("/agents", h.listCommerceAgents)
		r.Get("/agents/{id}", h.getCommerceAgent)
		r.Post("/agents/{id}/disable", h.setCommerceAgentStatus(commerce.Disabled))
		r.Post("/agents/{id}/enable", h.setCommerceAgentStatus(commerce.Active))
		r.With(h.limitMerchant(app.RuleCommerceWritePerMerchant)).Post("/funding-methods", h.registerFundingMethod)
		r.Get("/funding-methods/{id}", h.getFundingMethod)
		r.Post("/funding-methods/{id}/disable", h.setFundingMethodStatus(commerce.Disabled))
		r.Post("/funding-methods/{id}/enable", h.setFundingMethodStatus(commerce.Active))
		r.Get("/customers/{id}/funding-methods", h.listCustomerFundingMethods)
		r.With(h.limitMerchant(app.RuleCommerceWritePerMerchant)).Post("/mandate-requests", h.requestMandate)
		r.Get("/mandate-requests/{id}", h.getMandateRequest)
		r.Get("/mandates", h.listMandates)
		r.Get("/mandates/{id}", h.getMandate)
		r.Post("/mandates/{id}/revoke", h.revokeMandate)
		r.With(h.limitMerchant(app.RuleCommerceWritePerMerchant)).Post("/checkouts", h.submitCheckout)
		r.Get("/checkouts/{id}", h.getCheckout)
		r.Post("/checkouts/{id}/cancel", h.cancelCheckout)
		r.Post("/checkouts/{id}/grant", h.issueGrant)
		r.Post("/grants/redeem", h.redeemGrant)
		r.Get("/executions/{id}", h.getExecution)
		r.Get("/executions", h.listExecutions)
		r.Post("/executions/{id}/outcome", h.reportExecution(commerce.ReportDirect))
		r.Post("/executions/{id}/reconcile", h.reportExecution(commerce.ReportReconciliation))
		r.Get("/checkouts", h.listCheckouts)
	})
	// The customer's consent page, with the link Enquoir emailed them:
	// the token is the only credential, and no merchant ever holds it.
	r.Post("/v1/consent/view", h.viewConsent)
	r.Post("/v1/consent/approve", h.decideConsent(commerce.ConsentApproved))
	r.Post("/v1/consent/reject", h.decideConsent(commerce.ConsentRejected))
	r.Post("/v1/consent/revocation/view", h.viewRevocation)
	r.Post("/v1/consent/revocation/confirm", h.confirmRevocation)
	r.Post("/v1/consent/checkout/view", h.viewCheckoutApproval)
	r.Post("/v1/consent/checkout/approve", h.decideCheckoutApproval(true))
	r.Post("/v1/consent/checkout/reject", h.decideCheckoutApproval(false))

	r.With(h.limitIP(app.RuleSignUpPerIP)).Post("/v1/auth/signup", h.signUp)
	r.With(h.limitIP(app.RuleLoginPerIP)).Post("/v1/auth/login", h.logIn)
	r.Post("/v1/auth/logout", h.logOut)
	r.With(h.requireUser).Get("/v1/auth/session", h.getSession)
	r.With(h.requireUser).Post("/v1/auth/switch-organisation", h.switchOrganisation)

	r.Post("/v1/auth/verify-email", h.verifyEmail)
	r.With(h.requireUser).Post("/v1/auth/verify-email/resend", h.resendVerification)
	r.Post("/v1/auth/password/forgot", h.forgotPassword)
	r.Post("/v1/auth/password/reset", h.resetPassword)
	r.With(h.requireUser).Post("/v1/auth/password/change", h.changePassword)
	r.With(h.requireUser).Get("/v1/auth/sessions", h.listSessions)
	r.With(h.requireUser).Get("/v1/auth/activity", h.listMyActivity)
	r.With(h.requireUser).Delete("/v1/auth/sessions", h.revokeOtherSessions)
	r.With(h.requireUser).Delete("/v1/auth/sessions/{id}", h.revokeSession)
	r.Post("/v1/invitations/accept", h.acceptInvitation)

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

		manage := h.allow(domain.PermManageMembers)
		r.With(read).Get("/v1/members", h.listMembers)
		r.With(manage).Patch("/v1/members/{id}", h.changeMemberRole)
		// Anyone may leave; removing others is checked against roles.
		r.With(read).Delete("/v1/members/{id}", h.removeMember)
		r.With(manage, verified).Post("/v1/invitations", h.inviteMember)
		r.With(manage).Get("/v1/invitations", h.listInvitations)
		r.With(manage).Delete("/v1/invitations/{id}", h.revokeInvitation)

		r.With(h.allow(domain.PermViewAuditLog)).Get("/v1/audit-log", h.listAuditLog)

		// Connecting merchants issues credentials: owners and admins,
		// verified. Disabling or revoking needs no verification - cutting
		// access off is always allowed.
		integrations := h.allow(domain.PermManageIntegrations)
		r.With(integrations, verified).Post("/v1/merchant-integrations", h.createIntegration)
		r.With(read).Get("/v1/merchant-integrations", h.listIntegrations)
		r.With(read).Get("/v1/merchant-integrations/{id}", h.getIntegration)
		r.With(integrations).Post("/v1/merchant-integrations/{id}/disable", h.setIntegrationStatus(commerce.Disabled))
		r.With(integrations, verified).Post("/v1/merchant-integrations/{id}/enable", h.setIntegrationStatus(commerce.Active))
		r.With(integrations, verified).Post("/v1/merchant-integrations/{id}/rotate-key", h.rotateIntegrationKey)
		r.With(integrations).Delete("/v1/merchant-integrations/{id}/key", h.revokeIntegrationKey)
		// A merchant's commerce activity, read-only, for any member.
		r.With(read).Get("/v1/merchant-integrations/{id}/checkouts", h.dashboardMerchant(h.listCheckoutsFor))
		r.With(read).Get("/v1/merchant-integrations/{id}/mandates", h.dashboardMerchant(h.listMandatesFor))
		r.With(read).Get("/v1/merchant-integrations/{id}/executions", h.dashboardMerchant(h.listExecutionsFor))
	})
	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
