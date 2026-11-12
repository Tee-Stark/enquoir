package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// MandateUseCases are the mandate and consent use cases.
type MandateUseCases struct {
	Request    *commerceapp.RequestMandate
	GetRequest *commerceapp.GetMandateRequest
	View       *commerceapp.ViewConsent
	Decide     *commerceapp.DecideConsent
	List       *commerceapp.ListMandates
	Get        *commerceapp.GetMandate
	Revoke     *commerceapp.RequestMandateRevocation
	ViewRevoke *commerceapp.ViewRevocation
	Confirm    *commerceapp.ConfirmRevocation
}

type namedRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type mandateFundingDTO struct {
	ID       string  `json:"id"`
	Brand    string  `json:"brand"`
	LastFour *string `json:"last_four"`
}

type mandateScopeDTO struct {
	Currency          string   `json:"currency"`
	MaxTransaction    int64    `json:"max_transaction_minor"`
	MaxCumulative     int64    `json:"max_cumulative_minor"`
	MaxTransactions   *int     `json:"max_transactions"`
	AllowedCategories []string `json:"allowed_categories"`
}

type mandateTermsDTO struct {
	Merchant     namedRefDTO       `json:"merchant"`
	CustomerID   string            `json:"customer_id"`
	Agent        namedRefDTO       `json:"agent"`
	Funding      mandateFundingDTO `json:"funding_method"`
	ApprovalMode string            `json:"approval_mode"`
	Scope        mandateScopeDTO   `json:"scope"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Hash         string            `json:"terms_hash"`
}

func newMandateTermsDTO(t commerce.MandateTerms) mandateTermsDTO {
	p := t.Parties
	var count *int
	if t.Scope.MaxTransactions > 0 {
		n := t.Scope.MaxTransactions
		count = &n
	}
	cats := t.Scope.AllowedCategories
	if cats == nil {
		cats = []string{}
	}
	return mandateTermsDTO{
		Merchant: namedRefDTO{ID: string(p.MerchantID), Name: p.MerchantName}, CustomerID: string(p.CustomerID),
		Agent:        namedRefDTO{ID: string(p.AgentID), Name: p.AgentName},
		Funding:      mandateFundingDTO{ID: string(p.FundingMethodID), Brand: p.FundingDisplay.Brand, LastFour: optional(p.FundingDisplay.LastFour)},
		ApprovalMode: string(t.ApprovalMode),
		Scope: mandateScopeDTO{Currency: string(t.Scope.Currency), MaxTransaction: t.Scope.MaxPerTransactionMinor,
			MaxCumulative: t.Scope.MaxTotalMinor, MaxTransactions: count, AllowedCategories: cats},
		ExpiresAt: t.ExpiresAt.UTC(), Hash: t.Hash(),
	}
}

type mandateRequestDTO struct {
	ID         string          `json:"id"`
	ExternalID string          `json:"external_id"`
	Status     string          `json:"status"`
	Terms      mandateTermsDTO `json:"terms"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	DecidedAt  *time.Time      `json:"decided_at"`
	MandateID  *string         `json:"mandate_id"`
}

func newMandateRequestDTO(r commerce.MandateRequest) mandateRequestDTO {
	return mandateRequestDTO{ID: string(r.ID), ExternalID: string(r.ExternalID), Status: string(r.Status), Terms: newMandateTermsDTO(r.Terms),
		CreatedAt: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), DecidedAt: utcPtr(r.DecidedAt), MandateID: optional(string(r.MandateID))}
}

type mandateDTO struct {
	ID              string          `json:"id"`
	RequestID       string          `json:"request_id"`
	ConsentRecordID string          `json:"consent_record_id"`
	Status          string          `json:"status"`
	Terms           mandateTermsDTO `json:"terms"`
	ApprovedAt      time.Time       `json:"approved_at"`
	RevokedAt       *time.Time      `json:"revoked_at"`
	// Set while a revocation awaits the customer's confirmation.
	RevocationExpiresAt *time.Time `json:"revocation_expires_at"`
}

func newMandateDTO(m commerce.Mandate, status commerce.MandateStatus) mandateDTO {
	d := mandateDTO{ID: string(m.ID), RequestID: string(m.RequestID), ConsentRecordID: string(m.ConsentRecordID), Status: string(status),
		Terms: newMandateTermsDTO(m.Terms), ApprovedAt: m.ApprovedAt.UTC(), RevokedAt: utcPtr(m.RevokedAt)}
	if status == commerce.MandateRevocationPending {
		d.RevocationExpiresAt = utcPtr(m.RevocationExpiresAt)
	}
	return d
}

func (h *Handler) requestMandate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExternalID      string `json:"external_id"`
		CustomerID      string `json:"customer_id"`
		AgentID         string `json:"agent_id"`
		FundingMethodID string `json:"funding_method_id"`
		ApprovalMode    string `json:"approval_mode"`
		Scope           struct {
			Currency          string   `json:"currency"`
			MaxTransaction    int64    `json:"max_transaction_minor"`
			MaxCumulative     int64    `json:"max_cumulative_minor"`
			MaxTransactions   int      `json:"max_transactions"`
			AllowedCategories []string `json:"allowed_categories"`
		} `json:"scope"`
		ExpiresAt string `json:"expires_at"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(req.ExpiresAt))
	if err != nil {
		writeAppError(w, &app.ValidationError{Field: "expires_at", Message: "expires_at must be an RFC 3339 timestamp, e.g. 2026-12-11T09:00:00Z"}, "")
		return
	}
	mr, created, err := h.Mandates.Request.Run(r.Context(), merchantFrom(r.Context()), commerceapp.MandateRequestCommand{
		ExternalID: req.ExternalID, CustomerID: commerce.CustomerID(req.CustomerID), AgentID: commerce.AgentID(req.AgentID),
		FundingMethodID: commerce.FundingMethodID(req.FundingMethodID), ApprovalMode: req.ApprovalMode,
		Currency: req.Scope.Currency, MaxPerTransactionMinor: req.Scope.MaxTransaction, MaxTotalMinor: req.Scope.MaxCumulative,
		MaxTransactions: req.Scope.MaxTransactions, AllowedCategories: req.Scope.AllowedCategories, ExpiresAt: expires,
	})
	if err != nil {
		writeAppError(w, err, "could not request the mandate")
		return
	}
	writeJSON(w, registeredStatus(created), newMandateRequestDTO(mr))
}

func (h *Handler) getMandateRequest(w http.ResponseWriter, r *http.Request) {
	mr, err := h.Mandates.GetRequest.Run(r.Context(), merchantFrom(r.Context()), commerce.MandateRequestID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the mandate request")
		return
	}
	writeJSON(w, http.StatusOK, newMandateRequestDTO(mr))
}

func (h *Handler) listMandates(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.MandateFilter{CustomerID: commerce.CustomerID(q.str("customer_id")), Status: commerce.MandateStatus(q.str("status")),
		Limit: q.int("limit"), Cursor: q.str("cursor")}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.Mandates.List.Run(r.Context(), merchantFrom(r.Context()), f)
	if err != nil {
		writeAppError(w, err, "could not list mandates")
		return
	}
	out := make([]mandateDTO, len(page.Mandates))
	for i, m := range page.Mandates {
		out[i] = newMandateDTO(m.Mandate, m.Status)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) getMandate(w http.ResponseWriter, r *http.Request) {
	m, err := h.Mandates.Get.Run(r.Context(), merchantFrom(r.Context()), commerce.MandateID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the mandate")
		return
	}
	writeJSON(w, http.StatusOK, newMandateDTO(m.Mandate, m.Status))
}

func (h *Handler) revokeMandate(w http.ResponseWriter, r *http.Request) {
	m, err := h.Mandates.Revoke.Run(r.Context(), merchantFrom(r.Context()), commerce.MandateID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not revoke the mandate")
		return
	}
	writeJSON(w, http.StatusAccepted, newMandateDTO(m.Mandate, m.Status))
}

// The consent page's routes. They take no credential but the link's
// token, from the body (never the URL, which ends up in logs).
type consentViewDTO struct {
	RequestID string          `json:"request_id"`
	ExpiresAt time.Time       `json:"expires_at"`
	Terms     mandateTermsDTO `json:"terms"`
}

type consentOutcomeDTO struct {
	Request mandateRequestDTO `json:"request"`
	Mandate *mandateDTO       `json:"mandate"`
}

func (h *Handler) viewConsent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	mr, err := h.Mandates.View.Run(r.Context(), req.Token)
	if err != nil {
		writeAppError(w, err, "could not load the request")
		return
	}
	writeJSON(w, http.StatusOK, consentViewDTO{RequestID: string(mr.ID), ExpiresAt: mr.ExpiresAt.UTC(), Terms: newMandateTermsDTO(mr.Terms)})
}

func (h *Handler) decideConsent(decision commerce.ConsentDecision) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token     string `json:"token"`
			TermsHash string `json:"terms_hash"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		out, err := h.Mandates.Decide.Run(r.Context(), req.Token, req.TermsHash, decision)
		if err != nil {
			writeAppError(w, err, "could not record the decision")
			return
		}
		dto := consentOutcomeDTO{Request: newMandateRequestDTO(out.Request)}
		if out.Mandate != nil {
			m := newMandateDTO(*out.Mandate, commerce.MandateActive)
			dto.Mandate = &m
		}
		writeJSON(w, http.StatusOK, dto)
	}
}

func (h *Handler) viewRevocation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := h.Mandates.ViewRevoke.Run(r.Context(), req.Token)
	if err != nil {
		writeAppError(w, err, "could not load the mandate")
		return
	}
	writeJSON(w, http.StatusOK, newMandateDTO(m.Mandate, m.Status))
}

func (h *Handler) confirmRevocation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := h.Mandates.Confirm.Run(r.Context(), req.Token)
	if err != nil {
		writeAppError(w, err, "could not revoke the mandate")
		return
	}
	writeJSON(w, http.StatusOK, newMandateDTO(m.Mandate, m.Status))
}
