package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type submitIntentRequest struct {
	Payee     string `json:"payee"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	Reference string `json:"reference"`
	Reason    string `json:"reason"`
}

type submitIntentResponse struct {
	Key   string `json:"key"`
	State string `json:"state"`
}

func (h *Handler) submitIntent(w http.ResponseWriter, r *http.Request) {
	var req submitIntentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, codeInvalidJSON, "the request body is not valid JSON")
		return
	}

	amount, err := domain.ParseMajorUnits(req.Amount, domain.Currency(strings.ToUpper(strings.TrimSpace(req.Currency))))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, codeValidationFailed, err.Error())
		return
	}

	agent := agentFrom(r.Context())
	intent, err := domain.NewIntent(agent.OrgID, req.Payee, amount.MinorUnits(), string(amount.Currency()), req.Reference, req.Reason)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, codeValidationFailed, err.Error())
		return
	}
	intent.Agent = agent.AgentID

	result, err := h.Submit.Run(r.Context(), intent)
	if err != nil {
		writeAppError(w, err, "could not submit intent")
		return
	}

	writeJSON(w, http.StatusOK, submitIntentResponse{
		Key:   string(result.Key),
		State: string(result.State),
	})
}

type eventDTO struct {
	SequenceNumber int    `json:"sequence_number"`
	Type           string `json:"type"`
	Payload        string `json:"payload"`
}

type getIntentResponse struct {
	Key    string     `json:"key"`
	State  string     `json:"state"`
	Events []eventDTO `json:"events"`
}

func (h *Handler) getIntent(w http.ResponseWriter, r *http.Request) {
	key := domain.IntentKey(chi.URLParam(r, "key"))
	org := agentFrom(r.Context()).OrgID

	result, err := h.Get.Run(r.Context(), org, key)
	if errors.Is(err, app.ErrIntentNotFound) {
		writeProblem(w, http.StatusNotFound, codeNotFound, "intent not found")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, codeInternal, "could not load intent")
		return
	}

	events := make([]eventDTO, len(result.Events))
	for i, ev := range result.Events {
		events[i] = eventDTO{SequenceNumber: ev.SequenceNumber, Type: ev.Type, Payload: ev.Payload}
	}

	writeJSON(w, http.StatusOK, getIntentResponse{
		Key:    string(result.Key),
		State:  string(result.State),
		Events: events,
	})
}
