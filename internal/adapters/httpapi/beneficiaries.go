package httpapi

import (
	"errors"
	"net/http"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type registerBeneficiaryRequest struct {
	Name          string `json:"name"`
	AccountNumber string `json:"account_number"`
	RoutingNumber string `json:"routing_number"`
	Category      string `json:"account_category"`
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
	if !decodeJSON(w, r, &req) {
		return
	}

	b, err := domain.NewBeneficiary(agentFrom(r.Context()).OrgID, req.Name, req.AccountNumber, req.RoutingNumber, domain.AccountCategory(req.Category), domain.BeneficiaryAddress{
		Street:   req.Address.Street,
		City:     req.Address.City,
		State:    req.Address.State,
		Postcode: req.Address.Postcode,
	})
	if err != nil {
		writeProblem(w, http.StatusBadRequest, codeValidationFailed, err.Error())
		return
	}

	result, err := h.Register.Run(r.Context(), b)
	if errors.Is(err, app.ErrRejected) {
		// The rail's own reason, so the caller can see which field to fix.
		writeProblem(w, http.StatusUnprocessableEntity, codeRailRejected, err.Error())
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, codeInternal, "could not register beneficiary")
		return
	}

	writeJSON(w, http.StatusOK, registerBeneficiaryResponse{
		ID:  result.ID,
		Key: string(result.Key),
	})
}
