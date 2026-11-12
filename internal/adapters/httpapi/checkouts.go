package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// CheckoutUseCases are the checkout evaluation use cases.
type CheckoutUseCases struct {
	Submit *commerceapp.SubmitCheckout
	Get    *commerceapp.GetCheckout
	Cancel *commerceapp.CancelCheckout
	View   *commerceapp.ViewCheckoutApproval
	Decide *commerceapp.DecideCheckoutApproval
}

type checkoutItemDTO struct {
	ProductID      string `json:"product_id"`
	Name           string `json:"name"`
	Category       string `json:"category"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

type checkoutLinesDTO struct {
	CustomerID      string            `json:"customer_id"`
	AgentID         string            `json:"agent_id"`
	FundingMethodID string            `json:"funding_method_id"`
	Currency        string            `json:"currency"`
	Items           []checkoutItemDTO `json:"items"`
	ShippingMinor   int64             `json:"shipping_minor"`
	TaxMinor        int64             `json:"tax_minor"`
	FeesMinor       int64             `json:"fees_minor"`
	TotalMinor      int64             `json:"total_minor"`
}

type decisionDTO struct {
	Allowed bool              `json:"allowed"`
	Reasons []commerce.Denial `json:"reasons"`
}

type checkoutDTO struct {
	ID                string           `json:"id"`
	ExternalID        string           `json:"external_id"`
	MandateID         string           `json:"mandate_id"`
	Status            string           `json:"status"`
	Decision          decisionDTO      `json:"decision"`
	Checkout          checkoutLinesDTO `json:"checkout"`
	Hash              string           `json:"checkout_hash"`
	CreatedAt         time.Time        `json:"created_at"`
	DecidedAt         *time.Time       `json:"decided_at"`
	ApprovalExpiresAt *time.Time       `json:"approval_expires_at"`
	CanceledAt        *time.Time       `json:"canceled_at"`
}

func newCheckoutDTO(v commerceapp.CheckoutView) checkoutDTO {
	s := v.Snapshot
	items := make([]checkoutItemDTO, len(s.Items))
	for i, it := range s.Items {
		items[i] = checkoutItemDTO{string(it.ProductID), it.Name, it.Category, it.Quantity, it.UnitPriceMinor}
	}
	reasons := v.Denials
	if reasons == nil {
		reasons = []commerce.Denial{}
	}
	d := checkoutDTO{
		ID: string(v.ID), ExternalID: string(s.ExternalID), MandateID: string(s.MandateID), Status: string(v.Status),
		Decision: decisionDTO{Allowed: v.Status == commerce.CheckoutAuthorized, Reasons: reasons},
		Checkout: checkoutLinesDTO{CustomerID: string(s.CustomerID), AgentID: string(s.AgentID), FundingMethodID: string(s.FundingMethodID),
			Currency: string(s.Currency), Items: items, ShippingMinor: s.ShippingMinor, TaxMinor: s.TaxMinor, FeesMinor: s.FeesMinor, TotalMinor: s.TotalMinor},
		Hash: s.Hash(), CreatedAt: v.CreatedAt.UTC(), DecidedAt: utcPtr(v.DecidedAt), CanceledAt: utcPtr(v.CanceledAt),
	}
	if v.Status == commerce.CheckoutAwaitingApproval || v.Status == commerce.CheckoutExpired {
		d.ApprovalExpiresAt = utcPtr(v.ApprovalExpiresAt)
	}
	return d
}

// readStrict reads the body and decodes it strictly: a checkout comes
// from the merchant's server, and any field Enquoir doesn't know - card
// data, a price an agent asserts - is refused, not ignored.
func readStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeProblem(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, fmt.Sprintf("the request body is over the %d-byte limit", MaxRequestBodyBytes))
		return false
	}
	if err == nil {
		err = decodeStrict(body, v, "")
	}
	var verr *app.ValidationError
	switch {
	case err == nil:
		return true
	case errors.As(err, &verr):
		writeAppError(w, err, "")
	default:
		writeProblem(w, http.StatusBadRequest, codeInvalidJSON, "the request body is not valid JSON")
	}
	return false
}

func (h *Handler) submitCheckout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExternalID      string           `json:"external_id"`
		MandateID       string           `json:"mandate_id"`
		CustomerID      string           `json:"customer_id"`
		AgentID         string           `json:"agent_id"`
		FundingMethodID string           `json:"funding_method_id"`
		Checkout        checkoutLinesDTO `json:"checkout"`
	}
	if !readStrict(w, r, &req) {
		return
	}
	items := make([]commerce.CheckoutItem, len(req.Checkout.Items))
	for i, it := range req.Checkout.Items {
		items[i] = commerce.CheckoutItem{ProductID: commerce.ExternalID(it.ProductID), Name: it.Name, Category: it.Category,
			Quantity: it.Quantity, UnitPriceMinor: it.UnitPriceMinor}
	}
	c, created, err := h.Checkouts.Submit.Run(r.Context(), merchantFrom(r.Context()), commerceapp.CheckoutCommand{
		ExternalID: req.ExternalID, MandateID: commerce.MandateID(req.MandateID), CustomerID: commerce.CustomerID(req.CustomerID),
		AgentID: commerce.AgentID(req.AgentID), FundingMethodID: commerce.FundingMethodID(req.FundingMethodID),
		Currency: req.Checkout.Currency, Items: items, ShippingMinor: req.Checkout.ShippingMinor, TaxMinor: req.Checkout.TaxMinor,
		FeesMinor: req.Checkout.FeesMinor, TotalMinor: req.Checkout.TotalMinor,
	})
	if err != nil {
		writeAppError(w, err, "could not evaluate the checkout")
		return
	}
	writeJSON(w, registeredStatus(created), newCheckoutDTO(c))
}

func (h *Handler) getCheckout(w http.ResponseWriter, r *http.Request) {
	c, err := h.Checkouts.Get.Run(r.Context(), merchantFrom(r.Context()), commerce.CheckoutID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the checkout")
		return
	}
	writeJSON(w, http.StatusOK, newCheckoutDTO(c))
}

func (h *Handler) cancelCheckout(w http.ResponseWriter, r *http.Request) {
	c, err := h.Checkouts.Cancel.Run(r.Context(), merchantFrom(r.Context()), commerce.CheckoutID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not cancel the checkout")
		return
	}
	writeJSON(w, http.StatusOK, newCheckoutDTO(c))
}

func (h *Handler) viewCheckoutApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := h.Checkouts.View.Run(r.Context(), req.Token)
	if err != nil {
		writeAppError(w, err, "could not load the checkout")
		return
	}
	writeJSON(w, http.StatusOK, newCheckoutDTO(c))
}

func (h *Handler) decideCheckoutApproval(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token        string `json:"token"`
			CheckoutHash string `json:"checkout_hash"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		c, err := h.Checkouts.Decide.Run(r.Context(), req.Token, req.CheckoutHash, approve)
		if err != nil {
			writeAppError(w, err, "could not record the decision")
			return
		}
		writeJSON(w, http.StatusOK, newCheckoutDTO(c))
	}
}
