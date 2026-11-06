package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type apiKeyDTO struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type agentDTO struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	APIKey    *apiKeyDTO `json:"api_key"`
}

type issuedKeyDTO struct {
	ID        string    `json:"id"`
	Prefix    string    `json:"prefix"`
	Secret    string    `json:"secret"`
	CreatedAt time.Time `json:"created_at"`
}

type issuedAgentDTO struct {
	Agent  agentDTO     `json:"agent"`
	APIKey issuedKeyDTO `json:"api_key"`
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func newAgentDTO(a app.Agent) agentDTO {
	d := agentDTO{ID: string(a.ID), Name: a.Name, Status: "active", CreatedAt: a.CreatedAt.UTC(), RevokedAt: utcPtr(a.RevokedAt)}
	if a.Revoked() {
		d.Status = "revoked"
	}
	if a.Key != nil {
		d.APIKey = &apiKeyDTO{ID: a.Key.ID, Prefix: a.Key.Prefix, CreatedAt: a.Key.CreatedAt.UTC(), LastUsedAt: utcPtr(a.Key.LastUsedAt)}
	}
	return d
}

func newIssuedAgentDTO(i app.IssuedAgent) issuedAgentDTO {
	return issuedAgentDTO{
		Agent:  newAgentDTO(i.Agent),
		APIKey: issuedKeyDTO{ID: i.Agent.Key.ID, Prefix: i.Agent.Key.Prefix, Secret: i.Secret, CreatedAt: i.Agent.Key.CreatedAt.UTC()},
	}
}

func agentIDParam(r *http.Request) domain.AgentID { return domain.AgentID(chi.URLParam(r, "id")) }

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	issued, err := h.CreateAgent.Run(r.Context(), userFrom(r.Context()).Organisation.ID, req.Name)
	if err != nil {
		writeAppError(w, err, "could not create agent")
		return
	}
	writeJSON(w, http.StatusCreated, newIssuedAgentDTO(issued))
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.ListAgents.Run(r.Context(), userFrom(r.Context()).Organisation.ID)
	if err != nil {
		writeAppError(w, err, "could not list agents")
		return
	}
	out := make([]agentDTO, len(agents))
	for i, a := range agents {
		out[i] = newAgentDTO(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.GetAgent.Run(r.Context(), userFrom(r.Context()).Organisation.ID, agentIDParam(r))
	if err != nil {
		writeAppError(w, err, "could not load agent")
		return
	}
	writeJSON(w, http.StatusOK, newAgentDTO(a))
}

func (h *Handler) rotateAgentKey(w http.ResponseWriter, r *http.Request) {
	issued, err := h.RotateAgentKey.Run(r.Context(), userFrom(r.Context()).Organisation.ID, agentIDParam(r))
	if err != nil {
		writeAppError(w, err, "could not rotate the key")
		return
	}
	writeJSON(w, http.StatusOK, newIssuedAgentDTO(issued))
}

func (h *Handler) revokeAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.RevokeAgent.Run(r.Context(), userFrom(r.Context()).Organisation.ID, agentIDParam(r)); err != nil {
		writeAppError(w, err, "could not revoke agent")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
