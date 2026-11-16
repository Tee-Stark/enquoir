package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// ExecutionUseCases are the execution tracking and listing use cases.
type ExecutionUseCases struct {
	Report        *commerceapp.ReportExecutionOutcome
	List          *commerceapp.ListExecutions
	ListCheckouts *commerceapp.ListCheckouts
	Dashboard     *commerceapp.MerchantForDashboard
}

type executionEventDTO struct {
	Sequence    int       `json:"sequence"`
	Kind        string    `json:"kind"`
	Source      *string   `json:"source"`
	Outcome     *string   `json:"outcome"`
	StatusAfter string    `json:"status_after"`
	Reference   *string   `json:"provider_payment_reference"`
	EventID     *string   `json:"provider_event_id"`
	AmountMinor *int64    `json:"amount_minor"`
	Currency    *string   `json:"currency"`
	Detail      *string   `json:"detail"`
	At          time.Time `json:"at"`
}

type executionDetailDTO struct {
	executionDTO
	History []executionEventDTO `json:"history"`
}

type reportResultDTO struct {
	executionDTO
	Result string `json:"result"`
}

func newExecutionEventDTO(ev commerce.ExecutionEvent) executionEventDTO {
	var amount *int64
	if ev.AmountMinor != 0 {
		a := ev.AmountMinor
		amount = &a
	}
	return executionEventDTO{Sequence: ev.Sequence, Kind: string(ev.Kind), Source: optional(string(ev.Source)), Outcome: optional(string(ev.Outcome)),
		StatusAfter: string(ev.StatusAfter), Reference: optional(ev.Reference), EventID: optional(ev.EventID), AmountMinor: amount,
		Currency: optional(string(ev.Currency)), Detail: optional(ev.Detail), At: ev.At.UTC()}
}

func (h *Handler) reportExecution(source commerce.ReportSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Outcome       string `json:"outcome"`
			Provider      string `json:"provider"`
			PaymentRef    string `json:"provider_payment_reference"`
			EventID       string `json:"provider_event_id"`
			AmountMinor   int64  `json:"amount_minor"`
			Currency      string `json:"currency"`
			FailureReason string `json:"failure_reason"`
		}
		if !readStrict(w, r, &req) {
			return
		}
		src := source
		if src == commerce.ReportDirect && req.EventID != "" {
			src = commerce.ReportWebhook
		}
		if src == commerce.ReportReconciliation && req.EventID != "" {
			writeProblemBody(w, problem{Status: http.StatusBadRequest, Code: codeValidationFailed, Field: "provider_event_id",
				Detail: "provider_event_id: a reconciliation is your own lookup; report a webhook event with POST .../outcome"})
			return
		}
		v, err := h.Executions.Report.Run(r.Context(), merchantFrom(r.Context()), commerce.ExecutionID(chi.URLParam(r, "id")), commerce.ExecutionReport{
			Source: src, Outcome: commerce.ExecutionStatus(req.Outcome), Provider: commerce.Provider(req.Provider),
			ProviderPaymentReference: req.PaymentRef, EventID: req.EventID, AmountMinor: req.AmountMinor,
			Currency: currency(req.Currency), FailureReason: req.FailureReason,
		})
		if err != nil {
			writeAppError(w, err, "could not record the outcome")
			return
		}
		writeJSON(w, http.StatusOK, reportResultDTO{executionDTO: newExecutionDTO(v.Execution), Result: string(v.Result)})
	}
}

func (h *Handler) listExecutionsFor(w http.ResponseWriter, r *http.Request, p commerceapp.MerchantPrincipal) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.ExecutionFilter{Status: commerce.ExecutionStatus(q.str("status")), MandateID: commerce.MandateID(q.str("mandate_id")),
		Limit: q.int("limit"), Cursor: q.str("cursor")}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.Executions.List.Run(r.Context(), p, f)
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

func (h *Handler) listCheckoutsFor(w http.ResponseWriter, r *http.Request, p commerceapp.MerchantPrincipal) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.CheckoutFilter{MandateID: commerce.MandateID(q.str("mandate_id")), CustomerID: commerce.CustomerID(q.str("customer_id")),
		Status: commerce.CheckoutStatus(q.str("status")), Limit: q.int("limit"), Cursor: q.str("cursor")}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.Executions.ListCheckouts.Run(r.Context(), p, f)
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

func (h *Handler) listExecutions(w http.ResponseWriter, r *http.Request) {
	h.listExecutionsFor(w, r, merchantFrom(r.Context()))
}

func (h *Handler) listCheckouts(w http.ResponseWriter, r *http.Request) {
	h.listCheckoutsFor(w, r, merchantFrom(r.Context()))
}

// dashboardMerchant reads a member's view of one of their
// organisation's integrations, by the {id} in the path.
func (h *Handler) dashboardMerchant(next func(http.ResponseWriter, *http.Request, commerceapp.MerchantPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := h.Executions.Dashboard.Run(r.Context(), userFrom(r.Context()).Organisation.ID, integrationIDParam(r))
		if err != nil {
			writeAppError(w, err, "could not load the merchant integration")
			return
		}
		next(w, r, p)
	}
}

func (h *Handler) listMandatesFor(w http.ResponseWriter, r *http.Request, p commerceapp.MerchantPrincipal) {
	r = r.WithContext(context.WithValue(r.Context(), merchantKey, p))
	h.listMandates(w, r)
}

func currency(s string) domain.Currency { return domain.Currency(s) }
