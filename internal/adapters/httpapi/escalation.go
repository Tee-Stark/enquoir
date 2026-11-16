package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/domain"
)

func (h *Handler) decideEscalatedPayment(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := domain.IntentKey(chi.URLParam(r, "key"))
		if err := h.DecideEscalatedPayment.Run(r.Context(), userFrom(r.Context()).Organisation.ID, key, approve); err != nil {
			writeAppError(w, err, "could not decide the payment")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
