package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type memberDTO struct {
	User     userDTO   `json:"user"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	members, err := h.ListMembers.Run(r.Context(), userFrom(r.Context()).Organisation.ID)
	if err != nil {
		writeAppError(w, err, "could not list members")
		return
	}
	out := make([]memberDTO, len(members))
	for i, m := range members {
		out[i] = memberDTO{
			User: userDTO{ID: string(m.User.ID), Email: string(m.User.Email), EmailVerified: m.User.EmailVerified, CreatedAt: m.User.CreatedAt.UTC()},
			Role: string(m.Role), JoinedAt: m.JoinedAt.UTC(),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	role, err := app.ParseRoleField(req.Role)
	if err == nil {
		err = h.ChangeMemberRole.Run(r.Context(), userFrom(r.Context()), domain.UserID(chi.URLParam(r, "id")), role)
	}
	if err != nil {
		writeAppError(w, err, "could not change the role")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	if err := h.RemoveMember.Run(r.Context(), userFrom(r.Context()), domain.UserID(chi.URLParam(r, "id"))); err != nil {
		writeAppError(w, err, "could not remove the member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type invitationDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invited_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newInvitationDTO(i app.Invitation) invitationDTO {
	return invitationDTO{ID: i.ID, Email: string(i.Email), Role: string(i.Role), InvitedBy: string(i.InvitedBy),
		CreatedAt: i.CreatedAt.UTC(), ExpiresAt: i.ExpiresAt.UTC()}
}

func (h *Handler) inviteMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	role, err := app.ParseRoleField(req.Role)
	var inv app.Invitation
	if err == nil {
		inv, err = h.InviteMember.Run(r.Context(), userFrom(r.Context()), req.Email, role)
	}
	if err != nil {
		writeAppError(w, err, "could not send the invitation")
		return
	}
	writeJSON(w, http.StatusCreated, newInvitationDTO(inv))
}

func (h *Handler) listInvitations(w http.ResponseWriter, r *http.Request) {
	invs, err := h.ListInvitations.Run(r.Context(), userFrom(r.Context()).Organisation.ID)
	if err != nil {
		writeAppError(w, err, "could not list invitations")
		return
	}
	out := make([]invitationDTO, len(invs))
	for i, inv := range invs {
		out[i] = newInvitationDTO(inv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	if err := h.RevokeInvitation.Run(r.Context(), userFrom(r.Context()), chi.URLParam(r, "id")); err != nil {
		writeAppError(w, err, "could not revoke the invitation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	acc, err := h.AcceptInvitation.Run(r.Context(), req.Token, req.Password)
	if err != nil {
		writeAppError(w, err, "could not accept the invitation")
		return
	}
	if acc.Session != nil {
		h.setSessionCookie(w, *acc.Session, h.AcceptInvitation.Clock.Now())
	}
	u := acc.User
	writeJSON(w, http.StatusOK, map[string]any{
		"organisation":    organisationDTO{ID: string(acc.Organisation.ID), Name: acc.Organisation.Name, CreatedAt: acc.Organisation.CreatedAt.UTC()},
		"role":            string(acc.Role),
		"user":            userDTO{ID: string(u.ID), Email: string(u.Email), EmailVerified: u.EmailVerified, CreatedAt: u.CreatedAt.UTC()},
		"account_created": acc.AccountCreated,
	})
}

type auditEventDTO struct {
	ID      string            `json:"id"`
	Action  string            `json:"action"`
	Actor   *refEmailDTO      `json:"actor"`
	Target  *targetDTO        `json:"target"`
	Details map[string]string `json:"details"`
	IP      *string           `json:"ip"`
	At      time.Time         `json:"at"`
}

type refEmailDTO struct {
	Type  string  `json:"type"`
	ID    string  `json:"id"`
	Email *string `json:"email"`
	Name  *string `json:"name"`
}

type targetDTO struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (h *Handler) listAuditLog(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := app.AuditFilter{Action: q.str("action"), Actor: domain.UserID(q.str("actor_id")),
		From: q.time("from"), To: q.time("to"), Limit: q.int("limit"), Cursor: q.str("cursor")}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.ListAuditLog.Run(r.Context(), userFrom(r.Context()).Organisation.ID, f)
	if err != nil {
		writeAppError(w, err, "could not list the audit log")
		return
	}
	out := make([]auditEventDTO, len(page.Events))
	for i, e := range page.Events {
		d := auditEventDTO{ID: e.ID, Action: e.Action, Details: e.Details, IP: optional(e.IP), At: e.At.UTC()}
		if d.Details == nil {
			d.Details = map[string]string{}
		}
		switch {
		case e.ActorAgent != "":
			d.Actor = &refEmailDTO{Type: "agent", ID: string(e.ActorAgent), Name: optional(e.AgentName)}
		case e.ActorUserID != "":
			d.Actor = &refEmailDTO{Type: "user", ID: string(e.ActorUserID), Email: optional(string(e.ActorEmail))}
		}
		if e.TargetType != "" {
			d.Target = &targetDTO{Type: e.TargetType, ID: e.TargetID}
		}
		out[i] = d
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}
