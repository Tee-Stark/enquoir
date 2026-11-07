package app

import (
	"context"
	"time"

	"enquoir/internal/domain"
)

// SessionInfo is a session as its owner sees it: never the token.
type SessionInfo struct {
	ID           string
	Organisation Organisation
	CreatedAt    time.Time
	ExpiresAt    time.Time
	Current      bool
}

// SessionLister lists and revokes a user's own sessions.
type SessionLister interface {
	// ActiveForUser lists user's unrevoked, unexpired sessions, newest first.
	ActiveForUser(ctx context.Context, user domain.UserID, now time.Time) ([]SessionInfo, error)
	// RevokeByID revokes user's session id; ErrNotFound unless it is
	// user's and active.
	RevokeByID(ctx context.Context, user domain.UserID, id string, at time.Time) error
	RevokeAllForUser(ctx context.Context, user domain.UserID, except string, at time.Time) error
}

// ListSessions lists the signed-in user's sessions, in every
// organisation, marking the one making the request.
type ListSessions struct {
	Sessions SessionLister
	Clock    Clock
}

func (l *ListSessions) Run(ctx context.Context, p UserPrincipal) ([]SessionInfo, error) {
	list, err := l.Sessions.ActiveForUser(ctx, p.User.ID, l.Clock.Now())
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Current = list[i].ID == p.SessionID
	}
	return list, nil
}

// RevokeSession signs out one of the user's own sessions.
type RevokeSession struct {
	Sessions SessionLister
	Clock    Clock
	Cache    *Cache
}

func (r *RevokeSession) Run(ctx context.Context, p UserPrincipal, id string) error {
	if err := r.Sessions.RevokeByID(ctx, p.User.ID, id, r.Clock.Now()); err != nil {
		return err
	}
	r.Cache.InvalidateUser(ctx, p.User.ID)
	return nil
}

// RevokeOtherSessions signs out every session but the current one.
type RevokeOtherSessions struct {
	Sessions SessionLister
	Clock    Clock
	Cache    *Cache
	Audit    *Audit
}

func (r *RevokeOtherSessions) Run(ctx context.Context, p UserPrincipal) error {
	if err := r.Sessions.RevokeAllForUser(ctx, p.User.ID, p.SessionID, r.Clock.Now()); err != nil {
		return err
	}
	r.Cache.InvalidateUser(ctx, p.User.ID)
	r.Audit.record(ctx, p.Organisation.ID, AuditSessionsRevoked, "user", string(p.User.ID), nil)
	return nil
}
