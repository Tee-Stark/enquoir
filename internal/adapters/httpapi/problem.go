package httpapi

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// Problem codes: stable, machine-readable reasons a client can branch
// on. The human-readable detail may change; these may not.
const (
	codeInvalidJSON          = "invalid_json"
	codeValidationFailed     = "validation_failed"
	codeNotFound             = "not_found"
	codeRailRejected         = "rail_rejected"
	codeInternal             = "internal_error"
	codeUnauthenticated      = "unauthenticated"
	codeInvalidCreds         = "invalid_credentials"
	codeEmailTaken           = "email_taken"
	codePayeeUnknown         = "payee_unknown"
	codeForbiddenOrigin      = "forbidden_origin"
	codeNotImplemented       = "not_implemented"
	codeBodyTooLarge         = "body_too_large"
	codeForbidden            = "forbidden"
	codeNoOrganisation       = "no_organisation"
	codeEmailUnverified      = "email_unverified"
	codeAlreadyVerified      = "already_verified"
	codeTokenInvalid         = "token_invalid"
	codeLastOwner            = "last_owner"
	codeAlreadyMember        = "already_member"
	codeRateLimited          = "rate_limited"
	codeAccountLocked        = "account_locked"
	codeRegistrationConflict = "registration_conflict"
	codeCustomerUnknown      = "customer_unknown"
	codeCustomerDisabled     = "customer_disabled"
	codeAgentUnknown         = "agent_unknown"
	codeAgentDisabled        = "agent_disabled"
	codeFundingUnknown       = "funding_method_unknown"
	codeFundingDisabled      = "funding_method_disabled"
	codeTermsMismatch        = "terms_mismatch"
	codeMandateUnknown       = "mandate_unknown"
	codeCheckoutMismatch     = "checkout_mismatch"
	codeNotCancelable        = "checkout_not_cancelable"
	codeGrantInvalid         = "grant_invalid"
	codeGrantExpired         = "grant_expired"
	codeGrantRevoked         = "grant_revoked"
	codeGrantRedeemed        = "grant_redeemed"
	codeGrantNotIssuable     = "grant_not_issuable"
	codeRedemptionMismatch   = "redemption_mismatch"
	codeMandateNotActive     = "mandate_not_active"
	codeReportMismatch       = "report_mismatch"
	codeOutcomeConflict      = "outcome_conflict"
	codeReconcileRequired    = "reconciliation_required"
	codeEventConflict        = "event_conflict"
)

// problem is an RFC 9457 problem details object, with a code extension.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Field  string `json:"field,omitempty"`
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	writeProblemBody(w, problem{Status: status, Code: code, Detail: detail})
}

func writeProblemBody(w http.ResponseWriter, p problem) {
	p.Type = "about:blank"
	p.Title = http.StatusText(p.Status)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p)
}

// writeAppError maps an application error to its problem response.
// Anything unrecognised is a 500 with internalDetail, never the raw
// error text, which may carry internals.
func writeAppError(w http.ResponseWriter, err error, internalDetail string) {
	var verr *app.ValidationError
	var limited *app.RateLimitedError
	var locked *app.AccountLockedError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", retryAfter(limited.RetryAfter))
		writeProblem(w, http.StatusTooManyRequests, codeRateLimited, limited.Error())
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", retryAfter(locked.RetryAfter))
		writeProblem(w, http.StatusTooManyRequests, codeAccountLocked, locked.Error()+"; reset your password to unlock it sooner")
	case errors.As(err, &verr):
		writeProblemBody(w, problem{Status: http.StatusBadRequest, Code: codeValidationFailed, Detail: verr.Error(), Field: verr.Field})
	case errors.Is(err, app.ErrUnauthenticated):
		writeProblem(w, http.StatusUnauthorized, codeUnauthenticated, "sign in, or send a valid API key")
	case errors.Is(err, app.ErrInvalidCredentials):
		writeProblem(w, http.StatusUnauthorized, codeInvalidCreds, err.Error())
	case errors.Is(err, app.ErrEmailUnverified):
		writeProblem(w, http.StatusForbidden, codeEmailUnverified, err.Error()+": follow the link we emailed, or ask for a new one")
	case errors.Is(err, app.ErrAlreadyVerified):
		writeProblem(w, http.StatusConflict, codeAlreadyVerified, err.Error())
	case errors.Is(err, app.ErrTokenInvalid):
		writeProblem(w, http.StatusBadRequest, codeTokenInvalid, err.Error())
	case errors.Is(err, domain.ErrRoleNotAllowed):
		writeProblem(w, http.StatusForbidden, codeForbidden, err.Error())
	case errors.Is(err, app.ErrLastOwner):
		writeProblem(w, http.StatusConflict, codeLastOwner, err.Error())
	case errors.Is(err, app.ErrAlreadyMember):
		writeProblem(w, http.StatusConflict, codeAlreadyMember, err.Error())
	case errors.Is(err, app.ErrForbidden):
		writeProblem(w, http.StatusForbidden, codeForbidden, err.Error())
	case errors.Is(err, app.ErrNoOrganisation):
		writeProblem(w, http.StatusForbidden, codeNoOrganisation, err.Error()+"; accept an invitation to join one")
	case errors.Is(err, app.ErrEmailTaken):
		writeProblem(w, http.StatusConflict, codeEmailTaken, err.Error())
	case errors.Is(err, app.ErrPayeeUnknown):
		writeProblem(w, http.StatusUnprocessableEntity, codePayeeUnknown,
			"the payee is not a beneficiary this organisation registered; register it with POST /v1/beneficiaries first")
	case errors.Is(err, commerceapp.ErrCustomerUnknown):
		writeProblem(w, http.StatusUnprocessableEntity, codeCustomerUnknown,
			"the customer is not one this merchant registered; register it with POST /v1/commerce/customers first")
	case errors.Is(err, commerceapp.ErrCustomerDisabled):
		writeProblem(w, http.StatusUnprocessableEntity, codeCustomerDisabled, "the customer is disabled; enable it first")
	case errors.Is(err, commerceapp.ErrAgentUnknown):
		writeProblem(w, http.StatusUnprocessableEntity, codeAgentUnknown, "the agent is not one this merchant registered; register it with POST /v1/commerce/agents first")
	case errors.Is(err, commerceapp.ErrAgentDisabled):
		writeProblem(w, http.StatusUnprocessableEntity, codeAgentDisabled, "the agent is disabled; enable it first")
	case errors.Is(err, commerceapp.ErrFundingMethodUnknown):
		writeProblem(w, http.StatusUnprocessableEntity, codeFundingUnknown, err.Error())
	case errors.Is(err, commerceapp.ErrFundingMethodDisabled):
		writeProblem(w, http.StatusUnprocessableEntity, codeFundingDisabled, "the funding method is disabled; enable it first")
	case errors.Is(err, commerce.ErrReportMismatch):
		writeProblem(w, http.StatusUnprocessableEntity, codeReportMismatch, err.Error())
	case errors.Is(err, commerce.ErrOutcomeConflict):
		writeProblem(w, http.StatusConflict, codeOutcomeConflict, err.Error())
	case errors.Is(err, commerce.ErrReconciliationRequired):
		writeProblem(w, http.StatusConflict, codeReconcileRequired, err.Error())
	case errors.Is(err, commerce.ErrEventConflict):
		writeProblem(w, http.StatusConflict, codeEventConflict, err.Error())
	case errors.Is(err, commerceapp.ErrGrantInvalid):
		writeProblem(w, http.StatusUnprocessableEntity, codeGrantInvalid, err.Error())
	case errors.Is(err, commerce.ErrGrantExpired):
		writeProblem(w, http.StatusUnprocessableEntity, codeGrantExpired, err.Error()+"; submit the checkout again under a new external ID")
	case errors.Is(err, commerce.ErrGrantRevoked):
		writeProblem(w, http.StatusUnprocessableEntity, codeGrantRevoked, err.Error())
	case errors.Is(err, commerceapp.ErrMandateNotActive):
		writeProblem(w, http.StatusUnprocessableEntity, codeMandateNotActive, err.Error())
	case errors.Is(err, commerce.ErrGrantRedeemed):
		writeProblem(w, http.StatusConflict, codeGrantRedeemed, err.Error())
	case errors.Is(err, commerceapp.ErrGrantNotIssuable):
		writeProblem(w, http.StatusConflict, codeGrantNotIssuable, err.Error())
	case errors.Is(err, commerce.ErrRedemptionMismatch):
		writeProblem(w, http.StatusConflict, codeRedemptionMismatch, err.Error())
	case errors.Is(err, commerceapp.ErrMandateUnknown):
		writeProblem(w, http.StatusUnprocessableEntity, codeMandateUnknown, err.Error())
	case errors.Is(err, commerceapp.ErrCheckoutMismatch):
		writeProblem(w, http.StatusConflict, codeCheckoutMismatch, err.Error())
	case errors.Is(err, commerce.ErrCheckoutNotCancelable):
		writeProblem(w, http.StatusConflict, codeNotCancelable, err.Error())
	case errors.Is(err, commerceapp.ErrTermsMismatch):
		writeProblem(w, http.StatusConflict, codeTermsMismatch, err.Error())
	case errors.Is(err, commerceapp.ErrRegistrationConflict):
		writeProblem(w, http.StatusConflict, codeRegistrationConflict, err.Error())
	case errors.Is(err, app.ErrNotFound):
		writeProblem(w, http.StatusNotFound, codeNotFound, "not found")
	default:
		writeProblem(w, http.StatusInternalServerError, codeInternal, internalDetail)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func retryAfter(d time.Duration) string {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}
