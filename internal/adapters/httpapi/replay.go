package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/domain"
)

type replayEventDTO struct {
	SequenceNumber int    `json:"sequence_number"`
	Payload        string `json:"payload"`
	Hash           string `json:"hash"`
}

type replayStepDTO struct {
	Index      int             `json:"index"`
	At         time.Time       `json:"at"`
	OffsetMS   int64           `json:"offset_ms"`
	DeltaMS    int64           `json:"delta_ms"`
	DurationMS *int64          `json:"duration_ms"`
	Type       string          `json:"type"`
	Actor      string          `json:"actor"`
	FromState  *string         `json:"from_state"`
	ToState    *string         `json:"to_state"`
	Outcome    *string         `json:"outcome"`
	Title      string          `json:"title"`
	Event      *replayEventDTO `json:"event"`
	Attempt    *attemptDTO     `json:"attempt"`
}

type replayDTO struct {
	Payment    paymentDTO      `json:"payment"`
	StartedAt  *time.Time      `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at"`
	DurationMS int64           `json:"duration_ms"`
	States     []string        `json:"states"`
	Chain      chainDTO        `json:"chain"`
	Steps      []replayStepDTO `json:"steps"`
}

func (h *Handler) replayPayment(w http.ResponseWriter, r *http.Request) {
	rp, err := h.ReplayPayment.Run(r.Context(), userFrom(r.Context()).Organisation.ID, domain.IntentKey(chi.URLParam(r, "key")))
	if err != nil {
		writeAppError(w, err, "could not build the replay")
		return
	}
	out := replayDTO{Payment: newPaymentDTO(rp.Payment), DurationMS: rp.DurationMS, States: rp.States,
		Chain: newChainDTO(rp.ChainError), Steps: make([]replayStepDTO, len(rp.Steps))}
	if out.States == nil {
		out.States = []string{}
	}
	if len(rp.Steps) > 0 {
		start, end := rp.StartedAt.UTC(), rp.EndedAt.UTC()
		out.StartedAt, out.EndedAt = &start, &end
	}
	for i, s := range rp.Steps {
		d := replayStepDTO{Index: s.Index, At: s.At.UTC(), OffsetMS: s.OffsetMS, DeltaMS: s.DeltaMS, DurationMS: s.DurationMS,
			Type: s.Type, Actor: s.Actor, FromState: optional(s.FromState), ToState: optional(s.ToState), Title: s.Title}
		if s.Event != nil {
			d.Event = &replayEventDTO{SequenceNumber: s.Event.SequenceNumber, Payload: s.Event.Payload, Hash: s.Event.Hash}
		}
		if s.Attempt != nil {
			a := newAttemptDTO(*s.Attempt)
			d.Attempt, d.Outcome = &a, optional(a.Outcome)
		}
		out.Steps[i] = d
	}
	writeJSON(w, http.StatusOK, out)
}
