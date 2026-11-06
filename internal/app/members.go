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
}

func (c *ChangeMemberRole) Run(ctx context.Context, p UserPrincipal, user domain.UserID, to domain.Role) error {
	org := p.Organisation.ID
	err := c.Members.ChangeRole(ctx, org, user, to, func(from domain.Role, owners int) error {
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
	return c.Sessions.RevokeForMember(ctx, org, user, c.Clock.Now())
}

// RemoveMember removes a member - or, when the target is the actor,
// lets them leave - and signs them out of the organisation.
type RemoveMember struct {
	Members  MemberStore
	Sessions MemberSessions
	Clock    Clock
}

func (r *RemoveMember) Run(ctx context.Context, p UserPrincipal, user domain.UserID) error {
	org := p.Organisation.ID
	err := r.Members.RemoveMember(ctx, org, user, func(from domain.Role, owners int) error {
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
	// AcceptInvitation marks it accepted and adds the membership - and,
	// when newUser is not nil, creates that user, verified - all or
	// nothing. ErrTokenInvalid unless pending; ErrAlreadyMember if the
	// user is already in the organisation.
	AcceptInvitation(ctx context.Context, id string, user domain.UserID, newUser *User, at time.Time) error
}

// InviteMember invites someone by email.
type InviteMember struct {
	Accounts    AccountStore
	Members     MemberStore
	Invitations InvitationStore
	Emails      *Emails
	IDs         IDGenerator
	Clock       Clock
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
}

func (r *RevokeInvitation) Run(ctx context.Context, p UserPrincipal, id string) error {
	return r.Invitations.RevokeInvitation(ctx, p.Organisation.ID, id, r.Clock.Now())
}

// AcceptedInvitation is the result of accepting: the organisation and
// role, and - only when the acceptance created the account - a session
// for it. An existing account signs in as usual.
type AcceptedInvitation struct {
	Organisation   Organisation
	Role           domain.Role
	User           User
	AccountCreated bool
	Session        *IssuedSession
}

// AcceptInvitation spends an invitation link. Clicking it proves the
// address, so a new account starts verified, and an existing one is
// marked verified too.
type AcceptInvitation struct {
	Tokens      TokenStore
	Invitations InvitationStore
	Accounts    AccountStore
	Users       UserStore
	Hasher      PasswordHasher
	IDs         IDGenerator
	Issuer      *SessionIssuer
	Clock       Clock
}

func (a *AcceptInvitation) Run(ctx context.Context, token, password string) (AcceptedInvitation, error) {
	now := a.Clock.Now()
	hash := HashSecret(token)
	// Look before spending: a new account's password is checked first,
	// so a rejected one doesn't cost the invitation.
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
	var newUser *User
	if created {
		if err := domain.ValidatePassword(password); err != nil {
			return AcceptedInvitation{}, invalid("password", err)
		}
		hashed, err := a.Hasher.Hash(password)
		if err != nil {
			return AcceptedInvitation{}, err
		}
		user = User{ID: domain.UserID(a.IDs.NewID("usr")), Email: inv.Email, PasswordHash: hashed, EmailVerified: true, CreatedAt: now}
		newUser = &user
	}

	if _, err := a.Tokens.Consume(ctx, hash, PurposeInvitation, now); err != nil {
		return AcceptedInvitation{}, err
	}
	if err := a.Invitations.AcceptInvitation(ctx, inv.ID, user.ID, newUser, now); err != nil {
		return AcceptedInvitation{}, err
	}
	if !created {
		if err := a.Users.MarkEmailVerified(ctx, user.ID, now); err != nil {
			return AcceptedInvitation{}, err
		}
		user.EmailVerified = true
	}

	m, err := a.Accounts.Membership(ctx, inv.OrgID, user.ID)
	if err != nil {
		return AcceptedInvitation{}, fmt.Errorf("reading the new membership: %w", err)
	}
	out := AcceptedInvitation{Organisation: m.Organisation, Role: m.Role, User: user, AccountCreated: created}
	if created {
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
