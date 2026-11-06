package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"enquoir/internal/domain"
)

// PasswordHasher hashes passwords with a slow, salted function.
type PasswordHasher interface {
	Hash(password string) (encoded string, err error)
	Verify(password, encoded string) (bool, error)
}

// IDGenerator mints unique, prefixed record IDs ("org_...", "usr_...").
type IDGenerator interface {
	NewID(prefix string) string
}

// SecretGenerator mints high-entropy, prefixed secrets: session tokens
// and API keys. Only their hash is ever stored.
type SecretGenerator interface {
	NewSecret(prefix string) string
}

// Organisation is one Enquoir Cloud tenant.
type Organisation struct {
	ID        domain.OrgID
	Name      string
	CreatedAt time.Time
}

// User is a person who signs in to the dashboard. For now every user
// is their organisation's owner; invitations and roles attach here.
type User struct {
	ID            domain.UserID
	OrgID         domain.OrgID
	Email         domain.Email
	PasswordHash  string
	EmailVerified bool
	CreatedAt     time.Time
}

// Session is a signed-in dashboard session, stored by token hash.
type Session struct {
	TokenHash string
	UserID    domain.UserID
	OrgID     domain.OrgID
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// AccountStore persists organisations and their users.
type AccountStore interface {
	// CreateOrganisationWithOwner stores both or neither. Returns
	// ErrEmailTaken if owner's email is already registered.
	CreateOrganisationWithOwner(ctx context.Context, org Organisation, owner User) error
	// UserByEmail returns ErrNotFound when no user has email.
	UserByEmail(ctx context.Context, email domain.Email) (User, error)
	// UserWithOrganisation returns ErrNotFound when no such user exists.
	UserWithOrganisation(ctx context.Context, id domain.UserID) (User, Organisation, error)
}

// SessionStore persists sessions by token hash.
type SessionStore interface {
	Create(ctx context.Context, s Session) error
	// Get returns ErrNotFound for an unknown hash; expiry and
	// revocation are the caller's to check.
	Get(ctx context.Context, tokenHash string) (Session, error)
	// Revoke marks a session revoked; revoking an unknown or already
	// revoked session is not an error.
	Revoke(ctx context.Context, tokenHash string, at time.Time) error
}

// IssuedSession is a new session: the plaintext token, returned once,
// to be set as a cookie, and when it stops working.
type IssuedSession struct {
	Token     string
	ExpiresAt time.Time
}

// SignedIn is the result of signing up or logging in.
type SignedIn struct {
	User         User
	Organisation Organisation
	Session      IssuedSession
}

// SessionIssuer creates sessions. Shared by sign-up and log-in.
type SessionIssuer struct {
	Sessions SessionStore
	Secrets  SecretGenerator
	Clock    Clock
	TTL      time.Duration
}

func (i *SessionIssuer) issue(ctx context.Context, u User) (IssuedSession, error) {
	token := i.Secrets.NewSecret("ses")
	now := i.Clock.Now()
	s := Session{TokenHash: HashSecret(token), UserID: u.ID, OrgID: u.OrgID, CreatedAt: now, ExpiresAt: now.Add(i.TTL)}
	if err := i.Sessions.Create(ctx, s); err != nil {
		return IssuedSession{}, fmt.Errorf("creating session: %w", err)
	}
	return IssuedSession{Token: token, ExpiresAt: s.ExpiresAt}, nil
}

// SignUpCommand is a sign-up request as typed by a person.
type SignUpCommand struct {
	Email            string
	Password         string
	OrganisationName string
}

// SignUp creates an organisation, its owner, and a first session.
// Email verification is a placeholder: the user starts unverified and
// nothing is gated on it yet.
type SignUp struct {
	Accounts AccountStore
	Hasher   PasswordHasher
	IDs      IDGenerator
	Clock    Clock
	Issuer   *SessionIssuer
}

func (s *SignUp) Run(ctx context.Context, cmd SignUpCommand) (SignedIn, error) {
	email, err := domain.NewEmail(cmd.Email)
	if err != nil {
		return SignedIn{}, invalid("email", err)
	}
	if err := domain.ValidatePassword(cmd.Password); err != nil {
		return SignedIn{}, invalid("password", err)
	}
	orgName, err := domain.NewName(cmd.OrganisationName)
	if err != nil {
		return SignedIn{}, invalid("organisation_name", err)
	}

	hash, err := s.Hasher.Hash(cmd.Password)
	if err != nil {
		return SignedIn{}, fmt.Errorf("hashing password: %w", err)
	}

	now := s.Clock.Now()
	org := Organisation{ID: domain.OrgID(s.IDs.NewID("org")), Name: orgName, CreatedAt: now}
	user := User{ID: domain.UserID(s.IDs.NewID("usr")), OrgID: org.ID, Email: email, PasswordHash: hash, CreatedAt: now}
	if err := s.Accounts.CreateOrganisationWithOwner(ctx, org, user); err != nil {
		return SignedIn{}, err
	}

	session, err := s.Issuer.issue(ctx, user)
	if err != nil {
		return SignedIn{}, err
	}
	return SignedIn{User: user, Organisation: org, Session: session}, nil
}

// DummyPasswordHash is verified against when no account matches, so a
// login for an unknown email costs the same as a wrong password.
const DummyPasswordHash = "pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// LogIn exchanges an email and password for a new session.
type LogIn struct {
	Accounts AccountStore
	Hasher   PasswordHasher
	Issuer   *SessionIssuer
}

func (l *LogIn) Run(ctx context.Context, email, password string) (SignedIn, error) {
	user, err := l.findUser(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return SignedIn{}, err
	}
	encoded := user.PasswordHash
	if errors.Is(err, ErrNotFound) {
		encoded = DummyPasswordHash
	}
	ok, verr := l.Hasher.Verify(password, encoded)
	if errors.Is(err, ErrNotFound) || verr != nil || !ok {
		return SignedIn{}, ErrInvalidCredentials
	}

	_, org, err := l.Accounts.UserWithOrganisation(ctx, user.ID)
	if err != nil {
		return SignedIn{}, err
	}
	session, err := l.Issuer.issue(ctx, user)
	if err != nil {
		return SignedIn{}, err
	}
	return SignedIn{User: user, Organisation: org, Session: session}, nil
}

func (l *LogIn) findUser(ctx context.Context, raw string) (User, error) {
	email, err := domain.NewEmail(raw)
	if err != nil {
		return User{}, ErrNotFound
	}
	return l.Accounts.UserByEmail(ctx, email)
}

// LogOut revokes a session. Logging out twice, or with an unknown
// token, succeeds.
type LogOut struct {
	Sessions SessionStore
	Clock    Clock
}

func (l *LogOut) Run(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return l.Sessions.Revoke(ctx, HashSecret(token), l.Clock.Now())
}

// UserPrincipal is a signed-in dashboard user and their organisation.
type UserPrincipal struct {
	User         User
	Organisation Organisation
	ExpiresAt    time.Time
}

// ResolveSession turns a session token into the signed-in user.
type ResolveSession struct {
	Sessions SessionStore
	Accounts AccountStore
	Clock    Clock
}

func (r *ResolveSession) Run(ctx context.Context, token string) (UserPrincipal, error) {
	if token == "" {
		return UserPrincipal{}, ErrUnauthenticated
	}
	s, err := r.Sessions.Get(ctx, HashSecret(token))
	if errors.Is(err, ErrNotFound) {
		return UserPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return UserPrincipal{}, err
	}
	if s.RevokedAt != nil || !r.Clock.Now().Before(s.ExpiresAt) {
		return UserPrincipal{}, ErrUnauthenticated
	}
	user, org, err := r.Accounts.UserWithOrganisation(ctx, s.UserID)
	if errors.Is(err, ErrNotFound) {
		return UserPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return UserPrincipal{}, err
	}
	return UserPrincipal{User: user, Organisation: org, ExpiresAt: s.ExpiresAt}, nil
}
