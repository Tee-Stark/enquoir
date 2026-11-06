package httpapi

import (
	"errors"
	"net/http"

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
	Key     string `json:"key"`
	State   string `json:"state"`
	Outcome string `json:"outcome"`
}

func (h *Handler) submitIntent(w http.ResponseWriter, r *http.Request) {
	var req submitIntentRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	agent := agentFrom(r.Context())
	result, err := h.Submit.Run(r.Context(), app.SubmitIntentCommand{
		OrgID: agent.OrgID, Agent: agent.AgentID,
		Payee: req.Payee, Amount: req.Amount, Currency: req.Currency, Reference: req.Reference, Reason: req.Reason,
	})
	if err != nil {
		writeAppError(w, err, "could not submit intent")
		return
	}

	writeJSON(w, http.StatusOK, submitIntentResponse{
		Key:     string(result.Key),
		State:   string(result.State),
		Outcome: string(result.Outcome),
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
