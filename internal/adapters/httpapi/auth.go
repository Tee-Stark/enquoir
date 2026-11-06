package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"enquoir/internal/app"
)

// SessionCookieName is the dashboard session cookie.
const SessionCookieName = "enquoir_session"

// CookieSettings configures the session cookie. Secure must be true
// anywhere but plain-HTTP local development.
type CookieSettings struct {
	Secure bool
}

type userDTO struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

type organisationDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type sessionDTO struct {
	ExpiresAt time.Time `json:"expires_at"`
}

type authResponse struct {
	User         userDTO         `json:"user"`
	Organisation organisationDTO `json:"organisation"`
	Session      sessionDTO      `json:"session"`
}

func newAuthResponse(u app.User, o app.Organisation, expires time.Time) authResponse {
	return authResponse{
		User:         userDTO{ID: string(u.ID), Email: string(u.Email), EmailVerified: u.EmailVerified, CreatedAt: u.CreatedAt.UTC()},
		Organisation: organisationDTO{ID: string(o.ID), Name: o.Name, CreatedAt: o.CreatedAt.UTC()},
		Session:      sessionDTO{ExpiresAt: expires.UTC()},
	}
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, s app.IssuedSession, now time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    s.Token,
		Path:     "/",
		MaxAge:   int(s.ExpiresAt.Sub(now).Seconds()),
		HttpOnly: true,
		Secure:   h.Cookies.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.Cookies.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeProblem(w, http.StatusBadRequest, codeInvalidJSON, "the request body is not valid JSON")
		return false
	}
	return true
}

func (h *Handler) signUp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email            string `json:"email"`
		Password         string `json:"password"`
		OrganisationName string `json:"organisation_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	signed, err := h.SignUp.Run(r.Context(), app.SignUpCommand{Email: req.Email, Password: req.Password, OrganisationName: req.OrganisationName})
	if err != nil {
		writeAppError(w, err, "could not sign up")
		return
	}
	h.setSessionCookie(w, signed.Session, h.SignUp.Clock.Now())
	writeJSON(w, http.StatusCreated, newAuthResponse(signed.User, signed.Organisation, signed.Session.ExpiresAt))
}

func (h *Handler) logIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	signed, err := h.LogIn.Run(r.Context(), req.Email, req.Password)
	if err != nil {
		writeAppError(w, err, "could not log in")
		return
	}
	h.setSessionCookie(w, signed.Session, h.LogIn.Issuer.Clock.Now())
	writeJSON(w, http.StatusOK, newAuthResponse(signed.User, signed.Organisation, signed.Session.ExpiresAt))
}

func (h *Handler) logOut(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if err := h.LogOut.Run(r.Context(), c.Value); err != nil {
			writeAppError(w, err, "could not log out")
			return
		}
	}
	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	p := userFrom(r.Context())
	writeJSON(w, http.StatusOK, newAuthResponse(p.User, p.Organisation, p.ExpiresAt))
}

type ctxKey int

const (
	userKey ctxKey = iota
	agentKey
)

func userFrom(ctx context.Context) app.UserPrincipal {
	p, _ := ctx.Value(userKey).(app.UserPrincipal)
	return p
}

func agentFrom(ctx context.Context) app.AgentPrincipal {
	p, _ := ctx.Value(agentKey).(app.AgentPrincipal)
	return p
}

// requireUser admits only requests carrying a valid dashboard session.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookieName)
		if err != nil {
			writeAppError(w, app.ErrUnauthenticated, "")
			return
		}
		p, err := h.ResolveSession.Run(r.Context(), c.Value)
		if err != nil {
			writeAppError(w, err, "could not check the session")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, p)))
	})
}

// bearerToken extracts the token from a "Bearer <token>" Authorization
// header, or "" if the header is missing or a different scheme.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}

// requireAgent admits only requests carrying a valid agent API key, as
// a "Bearer <key>" Authorization header. This is how an AI agent
// authenticates, as distinct from requireUser's dashboard session.
func (h *Handler) requireAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.AuthenticateAgent.Run(r.Context(), bearerToken(r))
		if err != nil {
			writeAppError(w, err, "could not check the API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey, p)))
	})
}
