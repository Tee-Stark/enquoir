package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type registerBeneficiaryRequest struct {
	Name          string `json:"name"`
	AccountNumber string `json:"account_number"`
	RoutingNumber string `json:"routing_number"`
	Address       struct {
		Street   string `json:"street"`
		City     string `json:"city"`
		State    string `json:"state"`
		Postcode string `json:"postcode"`
	} `json:"address"`
}

type registerBeneficiaryResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

func (h *Handler) registerBeneficiary(w http.ResponseWriter, r *http.Request) {
	var req registerBeneficiaryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	b, err := domain.NewBeneficiary(req.Name, req.AccountNumber, req.RoutingNumber, domain.BeneficiaryAddress{
		Street:   req.Address.Street,
		City:     req.Address.City,
		State:    req.Address.State,
		Postcode: req.Address.Postcode,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.Register.Run(r.Context(), b)
	if errors.Is(err, app.ErrRejected) {
		// The rail's own reason, so the caller can see which field to fix.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not register beneficiary")
		return
	}

	writeJSON(w, http.StatusOK, registerBeneficiaryResponse{
		ID:  result.ID,
		Key: string(result.Key),
	})
}
