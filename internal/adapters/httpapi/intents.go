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
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	amount, err := domain.ParseMajorUnits(req.Amount, domain.Currency(strings.ToUpper(strings.TrimSpace(req.Currency))))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	intent, err := domain.NewIntent(req.Payee, amount.MinorUnits(), string(amount.Currency()), req.Reference, req.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.Submit.Run(r.Context(), intent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not submit intent")
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

	result, err := h.Get.Run(r.Context(), key)
	if errors.Is(err, app.ErrIntentNotFound) {
		writeError(w, http.StatusNotFound, "intent not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load intent")
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
