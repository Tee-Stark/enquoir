package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// Problem codes: stable, machine-readable reasons a client can branch
// on. The human-readable detail may change; these may not.
const (
	codeInvalidJSON      = "invalid_json"
	codeValidationFailed = "validation_failed"
	codeNotFound         = "not_found"
	codeRailRejected     = "rail_rejected"
	codeInternal         = "internal_error"
	codeUnauthenticated  = "unauthenticated"
	codeInvalidCreds     = "invalid_credentials"
	codeEmailTaken       = "email_taken"
	codePayeeUnknown     = "payee_unknown"
	codeForbiddenOrigin  = "forbidden_origin"
	codeNotImplemented   = "not_implemented"
	codeBodyTooLarge     = "body_too_large"
	codeForbidden        = "forbidden"
	codeNoOrganisation   = "no_organisation"
	codeEmailUnverified  = "email_unverified"
	codeAlreadyVerified  = "already_verified"
	codeTokenInvalid     = "token_invalid"
	codeLastOwner        = "last_owner"
	codeAlreadyMember    = "already_member"
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
	switch {
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
