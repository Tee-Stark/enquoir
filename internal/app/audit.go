package app

import (
	"context"
	"fmt"
	"time"

	"enquoir/internal/domain"
)

// Actor is who is acting in a request: set by the HTTP layer, read by
// the audit log.
type Actor struct {
	UserID    domain.UserID
	Email     domain.Email
	AgentID   domain.AgentID // when an agent acts, with its API key
	AgentName string
	// MerchantID is set when a merchant integration acts with its key
	// (commerce); treasury knows it only as an opaque ID.
	MerchantID   string
	MerchantName string
	IP           string
	UserAgent    string
}

type actorKey struct{}

// WithActor attaches the acting person to ctx.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the acting person, if any.
func ActorFrom(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey{}).(Actor)
	return a
}

// AuditEvent is one account action, recorded after it happened.
type AuditEvent struct {
	ID          string
	OrgID       domain.OrgID // empty for account-level events (a password reset)
	ActorUserID domain.UserID
	ActorEmail  domain.Email
	ActorAgent  domain.AgentID
	AgentName   string
	// ActorMerchant is the merchant integration that acted, with its key.
	ActorMerchant string
	MerchantName  string
	Action        string
	TargetType    string
	TargetID      string
	Details       map[string]string
	IP            string
	UserAgent     string
	At            time.Time
}

// Audit actions.
const (
	AuditLoggedIn           = "auth.logged_in"
	AuditLockedOut          = "auth.locked_out"
	AuditEmailVerified      = "auth.email_verified"
	AuditPasswordChanged    = "auth.password_changed"
	AuditPasswordReset      = "auth.password_reset"
	AuditSessionsRevoked    = "auth.sessions_revoked"
	AuditMemberInvited      = "member.invited"
	AuditInvitationRevoked  = "invitation.revoked"
	AuditInvitationAccepted = "invitation.accepted"
	AuditRoleChanged        = "member.role_changed"
	AuditMemberRemoved      = "member.removed"
	AuditMemberLeft         = "member.left"
	AuditAgentCreated       = "agent.created"
	AuditAgentKeyRotated    = "agent.key_rotated"
	AuditAgentRevoked       = "agent.revoked"
	AuditPayeeRegistered    = "payee.registered"

	// Commerce. Treasury records none of these; they are listed here so
	// the one audit log can filter by them.
	AuditIntegrationCreated         = "merchant_integration.created"
	AuditIntegrationDisabled        = "merchant_integration.disabled"
	AuditIntegrationEnabled         = "merchant_integration.enabled"
	AuditIntegrationKeyRotated      = "merchant_integration.key_rotated"
	AuditIntegrationKeyRevoked      = "merchant_integration.key_revoked"
	AuditCommerceCustomerRegistered = "commerce.customer.registered"
	AuditCommerceCustomerDisabled   = "commerce.customer.disabled"
	AuditCommerceCustomerEnabled    = "commerce.customer.enabled"
	AuditCommerceAgentRegistered    = "commerce.agent.registered"
	AuditCommerceAgentDisabled      = "commerce.agent.disabled"
	AuditCommerceAgentEnabled       = "commerce.agent.enabled"
)

// AuditFilter narrows the audit log. Zero fields don't filter.
type AuditFilter struct {
	Action   string
	Actor    domain.UserID
	From, To time.Time
	Limit    int
	Cursor   string
}

// AuditPage is a page of events, newest first.
type AuditPage struct {
	Events     []AuditEvent
	NextCursor string
}

// AuditLog stores audit events.
type AuditLog interface {
	Record(ctx context.Context, e AuditEvent) error
	List(ctx context.Context, org domain.OrgID, f AuditFilter) (AuditPage, error)
	// ListForUser lists events about user's own account - done by them,
	// or to them - in any organisation or none.
	ListForUser(ctx context.Context, user domain.UserID, f AuditFilter) (AuditPage, error)
}

// Audit records events. A nil *Audit records nothing. Events are
// written after the action they describe; a failed write does not undo
// the action (see docs/progress.md).
type Audit struct {
	Log   AuditLog
	IDs   IDGenerator
	Clock Clock
}

func (a *Audit) record(ctx context.Context, org domain.OrgID, action, targetType, targetID string, details map[string]string) {
	if a == nil || a.Log == nil {
		return
	}
	actor := ActorFrom(ctx)
	_ = a.Log.Record(ctx, AuditEvent{
		ID: a.IDs.NewID("aud"), OrgID: org, ActorUserID: actor.UserID, ActorEmail: actor.Email,
		ActorAgent: actor.AgentID, AgentName: actor.AgentName,
		ActorMerchant: actor.MerchantID, MerchantName: actor.MerchantName,
		Action: action, TargetType: targetType, TargetID: targetID, Details: details,
		IP: actor.IP, UserAgent: actor.UserAgent, At: a.Clock.Now(),
	})
}

// Record records an event in org, done by the actor in ctx. It is how
// another bounded context (commerce) writes to the one audit log.
func (a *Audit) Record(ctx context.Context, org domain.OrgID, action, targetType, targetID string, details map[string]string) {
	a.record(ctx, org, action, targetType, targetID, details)
}

// recordAs records with an explicit actor: for actions by someone not
// yet signed in (accepting an invitation, resetting a password).
func (a *Audit) recordAs(ctx context.Context, user User, org domain.OrgID, action, targetType, targetID string, details map[string]string) {
	actor := ActorFrom(ctx)
	actor.UserID, actor.Email = user.ID, user.Email
	a.record(WithActor(ctx, actor), org, action, targetType, targetID, details)
}

var knownAuditActions = map[string]bool{
	AuditLoggedIn: true, AuditLockedOut: true, AuditEmailVerified: true, AuditPasswordChanged: true, AuditPasswordReset: true,
	AuditSessionsRevoked: true, AuditMemberInvited: true, AuditInvitationRevoked: true, AuditInvitationAccepted: true,
	AuditRoleChanged: true, AuditMemberRemoved: true, AuditMemberLeft: true, AuditAgentCreated: true,
	AuditAgentKeyRotated: true, AuditAgentRevoked: true, AuditPayeeRegistered: true,
	AuditIntegrationCreated: true, AuditIntegrationDisabled: true, AuditIntegrationEnabled: true,
	AuditIntegrationKeyRotated: true, AuditIntegrationKeyRevoked: true,
	AuditCommerceCustomerRegistered: true, AuditCommerceCustomerDisabled: true, AuditCommerceCustomerEnabled: true,
	AuditCommerceAgentRegistered: true, AuditCommerceAgentDisabled: true, AuditCommerceAgentEnabled: true,
}

// ListMyActivity lists the events about the signed-in user's account:
// the only place account-level events (no organisation) are visible.
type ListMyActivity struct{ Log AuditLog }

func (l *ListMyActivity) Run(ctx context.Context, user domain.UserID, f AuditFilter) (AuditPage, error) {
	var err error
	if f.Limit, err = pageSize(f.Limit); err != nil {
		return AuditPage{}, err
	}
	if err := period(f.From, f.To); err != nil {
		return AuditPage{}, err
	}
	return l.Log.ListForUser(ctx, user, f)
}

// ListAuditLog lists an organisation's audit events.
type ListAuditLog struct{ Log AuditLog }

func (l *ListAuditLog) Run(ctx context.Context, org domain.OrgID, f AuditFilter) (AuditPage, error) {
	var err error
	if f.Limit, err = pageSize(f.Limit); err != nil {
		return AuditPage{}, err
	}
	if err := period(f.From, f.To); err != nil {
		return AuditPage{}, err
	}
	if f.Action != "" && !knownAuditActions[f.Action] {
		return AuditPage{}, &ValidationError{Field: "action", Message: fmt.Sprintf("unknown action %q", f.Action)}
	}
	return l.Log.List(ctx, org, f)
}
