package httpapi

import (
	"net/http"
	"net/url"
	"strings"
)

// AllowedOrigins are the browser origins (scheme://host[:port]) allowed
// to call the API with credentials: the dashboard frontends.
type AllowedOrigins []string

func (o AllowedOrigins) allows(origin string) bool {
	for _, a := range o {
		if strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

// ParseAllowedOrigins reads a comma-separated list.
func ParseAllowedOrigins(s string) AllowedOrigins {
	var out AllowedOrigins
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimRight(strings.TrimSpace(part), "/"); part != "" {
			out = append(out, part)
		}
	}
	return out
}

const (
	corsMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsHeaders = "Content-Type, Authorization"
)

// cors answers preflights and marks responses for allowed origins.
// Unlisted origins get no CORS headers, so browsers refuse them.
func (h *Handler) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		allowed := origin != "" && h.Origins.allows(origin)
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if allowed {
				w.Header().Set("Access-Control-Allow-Methods", corsMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkOrigin refuses state-changing requests a browser sends from an
// origin that is neither allowed nor the API's own host - cross-site
// request forgery, on top of the session cookie's SameSite=Lax.
// Requests without an Origin header (agents, curl) are not browsers
// acting for someone else, and pass.
func (h *Handler) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if origin != "" && !h.Origins.allows(origin) && !sameHost(origin, r.Host) {
				writeProblem(w, http.StatusForbidden, codeForbiddenOrigin, "requests from this origin are not allowed; add it to CORS_ALLOWED_ORIGINS")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

func notImplemented(what string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusNotImplemented, codeNotImplemented, what+" is not built yet; its requirements are pending")
	}
}
