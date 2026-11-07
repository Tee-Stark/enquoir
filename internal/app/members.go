package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"enquoir/internal/domain"
)

// InvitationTTL is how long an invitation works.
const InvitationTTL = 7 * 24 * time.Hour

var (
	// ErrLastOwner means the change would leave an organisation with no owner.
	ErrLastOwner = errors.New("an organisation must keep at least one owner")
	// ErrAlreadyMember means the person already belongs to the organisation.
	ErrAlreadyMember = errors.New("already a member of this organisation")
)

// Member is a user's membership, as listed.
type Member struct {
	User     User
	Role     domain.Role
	JoinedAt time.Time
}

// MembershipCheck decides a change to one membership, given its
// current role and the organisation's owner count, inside the store's
// lock on the organisation's memberships.
type MembershipCheck func(from domain.Role, owners int) error

// MemberStore changes memberships. ChangeRole and RemoveMember lock
// the organisation's memberships, run check, then apply - so two
// concurrent changes cannot both remove the last owner.
type MemberStore interface {
	ListMembers(ctx context.Context, org domain.OrgID) ([]Member, error)
	// ErrNotFound when user is not a member.
	ChangeRole(ctx context.Context, org domain.OrgID, user domain.UserID, to domain.Role, check MembershipCheck) error
	RemoveMember(ctx context.Context, org domain.OrgID, user domain.UserID, check MembershipCheck) error
}

// MemberSessions signs a member out of one organisation.
type MemberSessions interface {
	RevokeForMember(ctx context.Context, org domain.OrgID, user domain.UserID, at time.Time) error
}

// ListMembers lists an organisation's members.
type ListMembers struct{ Members MemberStore }

func (l *ListMembers) Run(ctx context.Context, org domain.OrgID) ([]Member, error) {
	return l.Members.ListMembers(ctx, org)
}

// ChangeMemberRole changes a member's role and signs them out of the
// organisation, so the new role applies at once.
type ChangeMemberRole struct {
	Members  MemberStore
	Sessions MemberSessions
	Clock    Clock
	Cache    *Cache
	Audit    *Audit
}

func (c *ChangeMemberRole) Run(ctx context.Context, p UserPrincipal, user domain.UserID, to domain.Role) error {
	org := p.Organisation.ID
	var was domain.Role
	err := c.Members.ChangeRole(ctx, org, user, to, func(from domain.Role, owners int) error {
		was = from
		if err := p.Role.CanAssign(from, to); err != nil {
			return err
		}
		if from == domain.Owner && to != domain.Owner && owners <= 1 {
			return ErrLastOwner
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.Cache.InvalidateUser(ctx, user)
	c.Audit.record(ctx, org, AuditRoleChanged, "user", string(user), map[string]string{"from": string(was), "to": string(to)})
	return c.Sessions.RevokeForMember(ctx, org, user, c.Clock.Now())
}

// RemoveMember removes a member - or, when the target is the actor,
// lets them leave - and signs them out of the organisation.
type RemoveMember struct {
	Members  MemberStore
	Sessions MemberSessions
	Clock    Clock
	Cache    *Cache
	Audit    *Audit
}

func (r *RemoveMember) Run(ctx context.Context, p UserPrincipal, user domain.UserID) error {
	org := p.Organisation.ID
	var was domain.Role
	err := r.Members.RemoveMember(ctx, org, user, func(from domain.Role, owners int) error {
		was = from
		if user != p.User.ID {
			// Removing someone is managing them: as if moving them down.
			if err := p.Role.CanAssign(from, domain.Viewer); err != nil {
				return err
			}
		}
		if from == domain.Owner && owners <= 1 {
			return ErrLastOwner
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.Cache.InvalidateUser(ctx, user)
	action := AuditMemberRemoved
	if user == p.User.ID {
		action = AuditMemberLeft
	}
	r.Audit.record(ctx, org, action, "user", string(user), map[string]string{"role": string(was)})
	return r.Sessions.RevokeForMember(ctx, org, user, r.Clock.Now())
}

// Invitation asks someone, by email, to join an organisation.
type Invitation struct {
	ID         string
	OrgID      domain.OrgID
	OrgName    string // filled in on reads
	Email      domain.Email
	Role       domain.Role
	InvitedBy  domain.UserID
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	RevokedAt  *time.Time
}

// Pending reports whether the invitation can still be accepted.
func (i Invitation) Pending(now time.Time) bool {
	return i.AcceptedAt == nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

// InvitationStore keeps invitations, scoped by organisation.
type InvitationStore interface {
	// CreateInvitation stores inv, revoking any pending invitation for
	// the same organisation and email.
	CreateInvitation(ctx context.Context, inv Invitation) error
	ListPendingInvitations(ctx context.Context, org domain.OrgID, now time.Time) ([]Invitation, error)
	// GetInvitation returns ErrNotFound for an unknown ID.
	GetInvitation(ctx context.Context, id string) (Invitation, error)
	// RevokeInvitation returns ErrNotFound unless the invitation is
	// org's and pending.
	RevokeInvitation(ctx context.Context, org domain.OrgID, id string, at time.Time) error
	// AcceptInvitation spends the invitation's link token, marks the
	// invitation accepted and adds the membership - creating the user,
	// or claiming an unverified one - all in one transaction.
	// ErrTokenInvalid unless the token and invitation are both live;
	// ErrAlreadyMember if the user is already in the organisation.
	AcceptInvitation(ctx context.Context, cmd AcceptInvitationCommand) error
}

// AcceptInvitationCommand is everything one acceptance changes.
type AcceptInvitationCommand struct {
	InvitationID string
	TokenHash    string
	UserID       domain.UserID
	// NewUser, when set, is created (verified).
	NewUser *User
	// ClaimPasswordHash, when set, replaces the password of the existing,
	// unverified user and verifies them. It applies only while the user
	// is still unverified; otherwise the acceptance fails and nothing
	// changes.
	ClaimPasswordHash string
	At                time.Time
}

// InviteMember invites someone by email.
type InviteMember struct {
	Accounts    AccountStore
	Members     MemberStore
	Invitations InvitationStore
	Emails      *Emails
	IDs         IDGenerator
	Clock       Clock
	Audit       *Audit
}

func (i *InviteMember) Run(ctx context.Context, p UserPrincipal, rawEmail string, role domain.Role) (Invitation, error) {
	email, err := domain.NewEmail(rawEmail)
	if err != nil {
		return Invitation{}, invalid("email", err)
	}
	if err := p.Role.CanInvite(role); err != nil {
		return Invitation{}, err
	}
	if existing, err := i.Accounts.UserByEmail(ctx, email); err == nil {
		if _, err := i.Accounts.Membership(ctx, p.Organisation.ID, existing.ID); err == nil {
			return Invitation{}, ErrAlreadyMember
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Invitation{}, err
	}

	now := i.Clock.Now()
	inv := Invitation{ID: i.IDs.NewID("inv"), OrgID: p.Organisation.ID, OrgName: p.Organisation.Name, Email: email, Role: role,
		InvitedBy: p.User.ID, CreatedAt: now, ExpiresAt: now.Add(InvitationTTL)}
	if err := i.Invitations.CreateInvitation(ctx, inv); err != nil {
		return Invitation{}, err
	}
	i.Audit.record(ctx, inv.OrgID, AuditMemberInvited, "invitation", inv.ID, map[string]string{"email": string(email), "role": string(role)})
	_, err = i.Emails.Send(ctx, Email{
		OrgID: p.Organisation.ID, To: email, Template: TemplateInvitation,
		Params: map[string]string{"organisation": p.Organisation.Name, "inviter": string(p.User.Email), "role": string(role)},
		Link:   &LinkSpec{Purpose: PurposeInvitation, Subject: inv.ID, TTL: InvitationTTL},
	})
	return inv, err
}

// ListInvitations lists an organisation's pending invitations.
type ListInvitations struct {
	Invitations InvitationStore
	Clock       Clock
}

func (l *ListInvitations) Run(ctx context.Context, org domain.OrgID) ([]Invitation, error) {
	return l.Invitations.ListPendingInvitations(ctx, org, l.Clock.Now())
}

// RevokeInvitation withdraws a pending invitation; its link stops working.
type RevokeInvitation struct {
	Invitations InvitationStore
	Clock       Clock
	Audit       *Audit
}

func (r *RevokeInvitation) Run(ctx context.Context, p UserPrincipal, id string) error {
	if err := r.Invitations.RevokeInvitation(ctx, p.Organisation.ID, id, r.Clock.Now()); err != nil {
		return err
	}
	r.Audit.record(ctx, p.Organisation.ID, AuditInvitationRevoked, "invitation", id, nil)
	return nil
}

// AcceptedInvitation is the result of accepting: the organisation and
// role, and - when the acceptance created or claimed the account - a
// session for it. An existing verified account signs in as usual.
type AcceptedInvitation struct {
	Organisation   Organisation
	Role           domain.Role
	User           User
	AccountCreated bool
	AccountClaimed bool
	Session        *IssuedSession
}

// AcceptInvitation spends an invitation link. Holding the link proves
// the invited address, so:
//
//   - no account: one is created with the given password, verified;
//   - an unverified account: it is claimed. Nobody ever proved that
//     account owns the address - anyone can sign up with someone else's
//     email - so the given password replaces its password, every one of
//     its sessions ends, and it is verified. Otherwise whoever
//     pre-registered the address would receive the invitation's access;
//   - a verified account: it already proved the address, and joins with
//     the link alone.
type AcceptInvitation struct {
	Tokens      TokenStore
	Invitations InvitationStore
	Accounts    AccountStore
	Sessions    SessionStore
	Hasher      PasswordHasher
	IDs         IDGenerator
	Issuer      *SessionIssuer
	Clock       Clock
	Cache       *Cache
	Audit       *Audit
}

func (a *AcceptInvitation) Run(ctx context.Context, token, password string) (AcceptedInvitation, error) {
	now := a.Clock.Now()
	hash := HashSecret(token)
	// Look before spending: a rejected password doesn't cost the link.
	peeked, err := a.Tokens.Peek(ctx, hash, PurposeInvitation, now)
	if err != nil {
		return AcceptedInvitation{}, err
	}
	inv, err := a.Invitations.GetInvitation(ctx, peeked.Subject)
	if err != nil || !inv.Pending(now) {
		return AcceptedInvitation{}, ErrTokenInvalid
	}

	user, err := a.Accounts.UserByEmail(ctx, inv.Email)
	created := errors.Is(err, ErrNotFound)
	if err != nil && !created {
		return AcceptedInvitation{}, err
	}
	claimed := !created && !user.EmailVerified
	cmd := AcceptInvitationCommand{InvitationID: inv.ID, TokenHash: hash, At: now}
	if created || claimed {
		if err := domain.ValidatePassword(password); err != nil {
			return AcceptedInvitation{}, invalid("password", err)
		}
		hashed, err := a.Hasher.Hash(password)
		if err != nil {
			return AcceptedInvitation{}, err
		}
		if created {
			user = User{ID: domain.UserID(a.IDs.NewID("usr")), Email: inv.Email, PasswordHash: hashed, CreatedAt: now}
			cmd.NewUser = &user
		} else {
			cmd.ClaimPasswordHash = hashed
		}
		user.EmailVerified = true
	}
	cmd.UserID = user.ID
	if err := a.Invitations.AcceptInvitation(ctx, cmd); err != nil {
		return AcceptedInvitation{}, err
	}
	if claimed {
		// Committed first, then the cache: never the other way round.
		if err := a.Sessions.RevokeAllForUser(ctx, user.ID, "", now); err != nil {
			return AcceptedInvitation{}, err
		}
		a.Cache.InvalidateUser(ctx, user.ID)
	}

	m, err := a.Accounts.Membership(ctx, inv.OrgID, user.ID)
	if err != nil {
		return AcceptedInvitation{}, fmt.Errorf("reading the new membership: %w", err)
	}
	a.Audit.recordAs(ctx, user, inv.OrgID, AuditInvitationAccepted, "invitation", inv.ID, map[string]string{
		"role": string(m.Role), "account_created": fmt.Sprint(created), "account_claimed": fmt.Sprint(claimed)})
	out := AcceptedInvitation{Organisation: m.Organisation, Role: m.Role, User: user, AccountCreated: created, AccountClaimed: claimed}
	if created || claimed {
		s, err := a.Issuer.issue(ctx, user, inv.OrgID)
		if err != nil {
			return AcceptedInvitation{}, err
		}
		out.Session = &s
	}
	return out, nil
}

// ParseRoleField parses a role from a request, naming the field.
func ParseRoleField(s string) (domain.Role, error) {
	r, err := domain.ParseRole(strings.TrimSpace(s))
	if err != nil {
		return "", invalid("role", err)
	}
	return r, nil
}
