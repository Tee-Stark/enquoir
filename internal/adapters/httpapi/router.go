// Package httpapi is the HTTP adapter: chi handlers that parse a
// request, call a use case, and respond. No business logic lives here.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
)

// Handler holds the use cases the API calls into.
type Handler struct {
	Submit *app.SubmitIntent
	Get    *app.GetIntent
}

func NewRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/healthz", h.healthz)
	r.Post("/v1/intents", h.submitIntent)
	r.Get("/v1/intents/{key}", h.getIntent)
	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
