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
	Submit   *app.SubmitIntent
	Get      *app.GetIntent
	Register *app.RegisterBeneficiary

	SignUp         *app.SignUp
	LogIn          *app.LogIn
	LogOut         *app.LogOut
	ResolveSession *app.ResolveSession
	Cookies        CookieSettings
}

func NewRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/healthz", h.healthz)
	r.Get("/openapi.yaml", serveSpec)
	r.Get("/docs", serveDocs)
	r.Post("/v1/intents", h.submitIntent)
	r.Get("/v1/intents/{key}", h.getIntent)
	r.Post("/v1/beneficiaries", h.registerBeneficiary)

	r.Post("/v1/auth/signup", h.signUp)
	r.Post("/v1/auth/login", h.logIn)
	r.Post("/v1/auth/logout", h.logOut)
	r.With(h.requireUser).Get("/v1/auth/session", h.getSession)
	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
