package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/commerce"
)

type checkoutReplayDecisionDTO struct {
	Status  string            `json:"status"`
	Reasons []commerce.Denial `json:"reasons"`
}

type checkoutReplayStepDTO struct {
	Index          int                        `json:"index"`
	At             time.Time                  `json:"at"`
	OffsetMS       int64                      `json:"offset_ms"`
	DeltaMS        int64                      `json:"delta_ms"`
	Type           string                     `json:"type"`
	Actor          string                     `json:"actor"`
	Title          string                     `json:"title"`
	Decision       *checkoutReplayDecisionDTO `json:"decision"`
	ExecutionEvent *executionEventDTO         `json:"execution_event"`
}

type checkoutReplayDTO struct {
	Checkout   checkoutDTO             `json:"checkout"`
	Execution  *executionDTO           `json:"execution"`
	StartedAt  *time.Time              `json:"started_at"`
	EndedAt    *time.Time              `json:"ended_at"`
	DurationMS int64                   `json:"duration_ms"`
	Steps      []checkoutReplayStepDTO `json:"steps"`
}

func (h *Handler) replayCheckout(w http.ResponseWriter, r *http.Request) {
	rp, err := h.OrgActivity.ReplayCheckout.Run(r.Context(), userFrom(r.Context()).Organisation.ID, commerce.CheckoutID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not build the replay")
		return
	}
	out := checkoutReplayDTO{Checkout: newCheckoutDTO(rp.Checkout), DurationMS: rp.DurationMS, Steps: make([]checkoutReplayStepDTO, len(rp.Steps))}
	if rp.Execution != nil {
		e := newExecutionDTO(*rp.Execution)
		out.Execution = &e
	}
	if len(rp.Steps) > 0 {
		start, end := rp.StartedAt.UTC(), rp.EndedAt.UTC()
		out.StartedAt, out.EndedAt = &start, &end
	}
	for i, s := range rp.Steps {
		d := checkoutReplayStepDTO{Index: s.Index, At: s.At.UTC(), OffsetMS: s.OffsetMS, DeltaMS: s.DeltaMS, Type: s.Type, Actor: s.Actor, Title: s.Title}
		if s.Decision != nil {
			reasons := s.Decision.Reasons
			if reasons == nil {
				reasons = []commerce.Denial{}
			}
			d.Decision = &checkoutReplayDecisionDTO{Status: string(s.Decision.Status), Reasons: reasons}
		}
		if s.ExecutionEvent != nil {
			ev := newExecutionEventDTO(*s.ExecutionEvent)
			d.ExecutionEvent = &ev
		}
		out.Steps[i] = d
	}
	writeJSON(w, http.StatusOK, out)
}
