package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// GrantUseCases are the authorization grant use cases.
type GrantUseCases struct {
	Issue  *commerceapp.IssueGrant
	Redeem *commerceapp.RedeemGrant
	Get    *commerceapp.GetExecution
}

type authorizationDTO struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Grant         *string    `json:"grant"`
	GrantIssuedAt *time.Time `json:"grant_issued_at"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RedeemedAt    *time.Time `json:"redeemed_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
	ExecutionID   *string    `json:"execution_id"`
}

func newAuthorizationDTO(a *commerce.Authorization, status commerce.AuthorizationStatus, grant string) *authorizationDTO {
	if a == nil {
		return nil
	}
	return &authorizationDTO{
		ID: string(a.ID), Status: string(status), Grant: optional(grant), GrantIssuedAt: utcPtr(a.GrantIssuedAt),
		CreatedAt: a.CreatedAt.UTC(), ExpiresAt: a.ExpiresAt.UTC(), RedeemedAt: utcPtr(a.RedeemedAt), RevokedAt: utcPtr(a.RevokedAt),
		ExecutionID: optional(string(a.ExecutionID)),
	}
}

type executionFundingDTO struct {
	ID        string  `json:"id"`
	Provider  string  `json:"provider"`
	Reference *string `json:"merchant_funding_method_id"`
	PSPRef    *string `json:"provider_method_reference"`
	Brand     string  `json:"brand"`
	LastFour  *string `json:"last_four"`
}

type executionDTO struct {
	ID                 string              `json:"id"`
	AuthorizationID    string              `json:"authorization_id"`
	CheckoutID         string              `json:"checkout_id"`
	ExternalCheckoutID string              `json:"external_checkout_id"`
	CheckoutHash       string              `json:"checkout_hash"`
	MandateID          string              `json:"mandate_id"`
	AmountMinor        int64               `json:"amount_minor"`
	Currency           string              `json:"currency"`
	FundingMethod      executionFundingDTO `json:"funding_method"`
	Status             string              `json:"status"`
	CreatedAt          time.Time           `json:"created_at"`
}

func newExecutionDTO(e commerce.Execution) executionDTO {
	f := e.FundingMethod
	return executionDTO{
		ID: string(e.ID), AuthorizationID: string(e.AuthorizationID), CheckoutID: string(e.CheckoutID),
		ExternalCheckoutID: string(e.ExternalCheckoutID), CheckoutHash: e.CheckoutHash, MandateID: string(e.MandateID),
		AmountMinor: e.AmountMinor, Currency: string(e.Currency), Status: string(e.Status), CreatedAt: e.CreatedAt.UTC(),
		FundingMethod: executionFundingDTO{ID: string(f.ID), Provider: string(f.Provider), Reference: optional(string(f.Reference)),
			PSPRef: optional(string(f.ProviderReference)), Brand: f.Display.Brand, LastFour: optional(f.Display.LastFour)},
	}
}

func (h *Handler) issueGrant(w http.ResponseWriter, r *http.Request) {
	c, err := h.Grants.Issue.Run(r.Context(), merchantFrom(r.Context()), commerce.CheckoutID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not issue the grant")
		return
	}
	writeJSON(w, http.StatusOK, newCheckoutDTO(c))
}

func (h *Handler) redeemGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Grant              string `json:"grant"`
		ExternalCheckoutID string `json:"external_checkout_id"`
		CheckoutHash       string `json:"checkout_hash"`
	}
	if !readStrict(w, r, &req) {
		return
	}
	e, created, err := h.Grants.Redeem.Run(r.Context(), merchantFrom(r.Context()), req.Grant, req.ExternalCheckoutID, req.CheckoutHash)
	if err != nil {
		writeAppError(w, err, "could not redeem the grant")
		return
	}
	writeJSON(w, registeredStatus(created), newExecutionDTO(e))
}

func (h *Handler) getExecution(w http.ResponseWriter, r *http.Request) {
	e, err := h.Grants.Get.Run(r.Context(), merchantFrom(r.Context()), commerce.ExecutionID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the execution")
		return
	}
	writeJSON(w, http.StatusOK, newExecutionDTO(e))
}
