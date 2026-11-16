package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// OrgActivityUseCases are the dashboard's organisation-wide commerce
// activity use cases: every checkout/execution across every merchant
// integration the organisation owns, parallel to ListPayments/
// ListAttempts on the treasury side. Named distinctly from
// ExecutionUseCases.List/ListCheckouts (merchant-key-authenticated,
// one merchant at a time) to keep the two surfaces from colliding.
type OrgActivityUseCases struct {
	ListCheckouts  *commerceapp.ListOrgCheckouts
	ListExecutions *commerceapp.ListOrgExecutions
	GetCheckout    *commerceapp.GetOrgCheckout
	GetExecution   *commerceapp.GetOrgExecution
	ReplayCheckout *commerceapp.ReplayCheckout
}

func (h *Handler) listOrgCheckouts(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.OrgCheckoutFilter{
		MandateID: commerce.MandateID(q.str("mandate_id")), CustomerID: commerce.CustomerID(q.str("customer_id")),
		AgentID: commerce.AgentID(q.str("agent_id")), Status: commerce.CheckoutStatus(q.str("status")),
		Currency: domain.Currency(strings.ToUpper(q.str("currency"))), From: q.time("from"), To: q.time("to"),
		Limit: q.int("limit"), Cursor: q.str("cursor"),
	}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.OrgActivity.ListCheckouts.Run(r.Context(), userFrom(r.Context()).Organisation.ID, f)
	if err != nil {
		writeAppError(w, err, "could not list checkouts")
		return
	}
	out := make([]checkoutDTO, len(page.Checkouts))
	for i, c := range page.Checkouts {
		out[i] = newCheckoutDTO(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) listOrgExecutions(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.OrgExecutionFilter{
		Status: commerce.ExecutionStatus(q.str("status")), MandateID: commerce.MandateID(q.str("mandate_id")),
		AgentID: commerce.AgentID(q.str("agent_id")), Currency: domain.Currency(strings.ToUpper(q.str("currency"))),
		From: q.time("from"), To: q.time("to"), Limit: q.int("limit"), Cursor: q.str("cursor"),
	}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.OrgActivity.ListExecutions.Run(r.Context(), userFrom(r.Context()).Organisation.ID, f)
	if err != nil {
		writeAppError(w, err, "could not list executions")
		return
	}
	out := make([]executionDTO, len(page.Executions))
	for i, e := range page.Executions {
		out[i] = newExecutionDTO(e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) getOrgCheckout(w http.ResponseWriter, r *http.Request) {
	v, err := h.OrgActivity.GetCheckout.Run(r.Context(), userFrom(r.Context()).Organisation.ID, commerce.CheckoutID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the checkout")
		return
	}
	writeJSON(w, http.StatusOK, newCheckoutDTO(v))
}

func (h *Handler) getOrgExecution(w http.ResponseWriter, r *http.Request) {
	d, err := h.OrgActivity.GetExecution.Run(r.Context(), userFrom(r.Context()).Organisation.ID, commerce.ExecutionID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the execution")
		return
	}
	hist := make([]executionEventDTO, len(d.History))
	for i, ev := range d.History {
		hist[i] = newExecutionEventDTO(ev)
	}
	writeJSON(w, http.StatusOK, executionDetailDTO{executionDTO: newExecutionDTO(d.Execution), History: hist})
}
