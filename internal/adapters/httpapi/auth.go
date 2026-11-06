package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/domain"
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
	User          userDTO         `json:"user"`
	Organisation  organisationDTO `json:"organisation"`
	Role          string          `json:"role"`
	Session       sessionDTO      `json:"session"`
	Organisations []myOrgDTO      `json:"organisations,omitempty"`
}

type myOrgDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func newAuthResponse(u app.User, o app.Organisation, role domain.Role, expires time.Time) authResponse {
	return authResponse{
		User:         userDTO{ID: string(u.ID), Email: string(u.Email), EmailVerified: u.EmailVerified, CreatedAt: u.CreatedAt.UTC()},
		Organisation: organisationDTO{ID: string(o.ID), Name: o.Name, CreatedAt: o.CreatedAt.UTC()},
		Role:         string(role),
		Session:      sessionDTO{ExpiresAt: expires.UTC()},
	}
}

func signedInResponse(s app.SignedIn) authResponse {
	return newAuthResponse(s.User, s.Organisation, s.Role, s.Session.ExpiresAt)
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

// decodeJSON reads a JSON request body into v, answering for the
// caller when it can't: 413 past the body limit, 400 when not JSON.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		writeProblem(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, fmt.Sprintf("the request body is over the %d-byte limit", MaxRequestBodyBytes))
	default:
		writeProblem(w, http.StatusBadRequest, codeInvalidJSON, "the request body is not valid JSON")
	}
	return false
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
	writeJSON(w, http.StatusCreated, signedInResponse(signed))
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
	writeJSON(w, http.StatusOK, signedInResponse(signed))
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
	ms, err := h.MyOrgs.Run(r.Context(), p.User.ID)
	if err != nil {
		writeAppError(w, err, "could not load your organisations")
		return
	}
	out := newAuthResponse(p.User, p.Organisation, p.Role, p.ExpiresAt)
	out.Organisations = make([]myOrgDTO, len(ms))
	for i, m := range ms {
		out.Organisations[i] = myOrgDTO{ID: string(m.Organisation.ID), Name: m.Organisation.Name, Role: string(m.Role)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) switchOrganisation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrganisationID string `json:"organisation_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, _ := r.Cookie(SessionCookieName)
	signed, err := h.SwitchOrg.Run(r.Context(), userFrom(r.Context()), c.Value, domain.OrgID(req.OrganisationID))
	if err != nil {
		writeAppError(w, err, "could not switch organisation")
		return
	}
	h.setSessionCookie(w, signed.Session, h.SwitchOrg.Issuer.Clock.Now())
	writeJSON(w, http.StatusOK, signedInResponse(signed))
}

// allow admits only a signed-in user whose role has p.
func (h *Handler) allow(p domain.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !userFrom(r.Context()).Can(p) {
				writeAppError(w, app.ErrForbidden, "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
		ctx := context.WithValue(r.Context(), userKey, p)
		actor := app.ActorFrom(ctx)
		actor.UserID, actor.Email = p.User.ID, p.User.Email
		next.ServeHTTP(w, r.WithContext(app.WithActor(ctx, actor)))
	})
}

// withActor records the client's IP and user agent for the audit log;
// requireUser adds who they are once the session resolves.
func (h *Handler) withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := app.WithActor(r.Context(), app.Actor{IP: clientIP(r, h.ClientIPHeader), UserAgent: r.UserAgent()})
		next.ServeHTTP(w, r.WithContext(ctx))
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

// requireVerified admits only a user whose email address is verified:
// issuing credentials, inviting people and registering payees.
func requireVerified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r.Context()).User.EmailVerified {
			writeAppError(w, app.ErrEmailUnverified, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.VerifyEmail.Run(r.Context(), req.Token); err != nil {
		writeAppError(w, err, "could not verify the email address")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resendVerification(w http.ResponseWriter, r *http.Request) {
	if err := h.ResendVerify.Run(r.Context(), userFrom(r.Context())); err != nil {
		writeAppError(w, err, "could not send the verification email")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.ForgotPassword.Run(r.Context(), req.Email); err != nil {
		writeAppError(w, err, "could not start a password reset")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.ResetPassword.Run(r.Context(), req.Token, req.Password); err != nil {
		writeAppError(w, err, "could not reset the password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.ChangePassword.Run(r.Context(), userFrom(r.Context()), req.Current, req.New); err != nil {
		writeAppError(w, err, "could not change the password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type sessionInfoDTO struct {
	ID           string          `json:"id"`
	Organisation organisationDTO `json:"organisation"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	Current      bool            `json:"current"`
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	list, err := h.ListSessions.Run(r.Context(), userFrom(r.Context()))
	if err != nil {
		writeAppError(w, err, "could not list sessions")
		return
	}
	out := make([]sessionInfoDTO, len(list))
	for i, s := range list {
		o := s.Organisation
		out[i] = sessionInfoDTO{ID: s.ID, Organisation: organisationDTO{ID: string(o.ID), Name: o.Name, CreatedAt: o.CreatedAt.UTC()},
			CreatedAt: s.CreatedAt.UTC(), ExpiresAt: s.ExpiresAt.UTC(), Current: s.Current}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	if err := h.RevokeSession.Run(r.Context(), userFrom(r.Context()), chi.URLParam(r, "id")); err != nil {
		writeAppError(w, err, "could not revoke the session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	if err := h.RevokeOtherSessions.Run(r.Context(), userFrom(r.Context())); err != nil {
		writeAppError(w, err, "could not revoke sessions")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentOrMember admits an agent by its API key, or else a signed-in,
// verified member whose role has perm.
func (h *Handler) agentOrMember(perm domain.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		asMember := h.requireUser(h.allow(perm)(requireVerified(next)))
		asAgent := h.requireAgent(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if bearerToken(r) != "" {
				asAgent.ServeHTTP(w, r)
				return
			}
			asMember.ServeHTTP(w, r)
		})
	}
}

// orgOf is the organisation acting: the agent's, or the member's.
func orgOf(ctx context.Context) domain.OrgID {
	if a := agentFrom(ctx); a.OrgID != "" {
		return a.OrgID
	}
	return userFrom(ctx).Organisation.ID
}
