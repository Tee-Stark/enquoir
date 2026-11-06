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

// User is a person who signs in to the dashboard. One user may belong
// to several organisations, with a role in each (Membership).
type User struct {
	ID            domain.UserID
	Email         domain.Email
	PasswordHash  string
	EmailVerified bool
	CreatedAt     time.Time
}

// Membership is a user's role in one organisation.
type Membership struct {
	Organisation Organisation
	UserID       domain.UserID
	Role         domain.Role
	CreatedAt    time.Time
}

// Session is a signed-in dashboard session, bound to one organisation
// and stored by token hash. ID names it for listing and revocation;
// the token is never stored.
type Session struct {
	ID        string
	TokenHash string
	UserID    domain.UserID
	OrgID     domain.OrgID
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// AccountStore persists users, organisations and memberships.
type AccountStore interface {
	// CreateOrganisationWithOwner stores the organisation, the user and
	// the user's owner membership, all or nothing. Returns
	// ErrEmailTaken if the email is already registered.
	CreateOrganisationWithOwner(ctx context.Context, org Organisation, owner User) error
	// UserByEmail and UserByID return ErrNotFound for no such user.
	UserByEmail(ctx context.Context, email domain.Email) (User, error)
	UserByID(ctx context.Context, id domain.UserID) (User, error)
	// Membership returns ErrNotFound when user is not in org.
	Membership(ctx context.Context, org domain.OrgID, user domain.UserID) (Membership, error)
	// MembershipsOf lists user's memberships, oldest first.
	MembershipsOf(ctx context.Context, user domain.UserID) ([]Membership, error)
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
	// RevokeAllForUser revokes every session of user, in every
	// organisation, except the one with ID except ("" for none).
	RevokeAllForUser(ctx context.Context, user domain.UserID, except string, at time.Time) error
}

// IssuedSession is a new session: the plaintext token, returned once,
// to be set as a cookie, and when it stops working.
type IssuedSession struct {
	Token     string
	ExpiresAt time.Time
}

// SignedIn is the result of signing up or logging in: the user, the
// organisation the new session is bound to, and the role there.
type SignedIn struct {
	User         User
	Organisation Organisation
	Role         domain.Role
	Session      IssuedSession
}

// SessionIssuer creates sessions. Shared by sign-up and log-in.
type SessionIssuer struct {
	Sessions SessionStore
	IDs      IDGenerator
	Secrets  SecretGenerator
	Clock    Clock
	TTL      time.Duration
}

func (i *SessionIssuer) issue(ctx context.Context, u User, org domain.OrgID) (IssuedSession, error) {
	token := i.Secrets.NewSecret("ses")
	now := i.Clock.Now()
	s := Session{ID: i.IDs.NewID("ses"), TokenHash: HashSecret(token), UserID: u.ID, OrgID: org, CreatedAt: now, ExpiresAt: now.Add(i.TTL)}
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

// SignUp creates an organisation, its owner, and a first session, and
// sends a verification email. The user starts unverified: they can
// look around, but cannot create agents or invite anyone until they
// verify.
type SignUp struct {
	Accounts AccountStore
	Hasher   PasswordHasher
	IDs      IDGenerator
	Clock    Clock
	Issuer   *SessionIssuer
	Emails   *Emails // nil: no email (tooling)
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
	user := User{ID: domain.UserID(s.IDs.NewID("usr")), Email: email, PasswordHash: hash, CreatedAt: now}
	if err := s.Accounts.CreateOrganisationWithOwner(ctx, org, user); err != nil {
		return SignedIn{}, err
	}

	if s.Emails != nil {
		if _, err := s.Emails.Send(ctx, verificationEmail(user)); err != nil {
			return SignedIn{}, err
		}
	}
	session, err := s.Issuer.issue(ctx, user, org.ID)
	if err != nil {
		return SignedIn{}, err
	}
	return SignedIn{User: user, Organisation: org, Role: domain.Owner, Session: session}, nil
}

// DummyPasswordHash is verified against when no account matches, so a
// login for an unknown email costs the same as a wrong password.
const DummyPasswordHash = "pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// LogIn exchanges an email and password for a new session. Attempts
// per email are rate limited, and LockoutThreshold failures lock the
// email for LockoutDuration - whether or not it has an account, so the
// lock reveals nothing.
type LogIn struct {
	Accounts AccountStore
	Hasher   PasswordHasher
	Issuer   *SessionIssuer
	Throttle *Throttle
	Audit    *Audit
}

func (l *LogIn) Run(ctx context.Context, rawEmail, password string) (SignedIn, error) {
	email, _ := domain.NewEmail(rawEmail)
	if email != "" {
		if err := l.Throttle.Check(ctx, RuleLoginPerEmail, string(email)); err != nil {
			return SignedIn{}, err
		}
		if left := l.Throttle.lockedFor(ctx, email); left > 0 {
			return SignedIn{}, &AccountLockedError{RetryAfter: left}
		}
	}

	user, err := l.findUser(ctx, rawEmail)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return SignedIn{}, err
	}
	encoded := user.PasswordHash
	if errors.Is(err, ErrNotFound) {
		encoded = DummyPasswordHash
	}
	ok, verr := l.Hasher.Verify(password, encoded)
	if errors.Is(err, ErrNotFound) || verr != nil || !ok {
		if email != "" && l.Throttle.failedLogIn(ctx, email) {
			if !errors.Is(err, ErrNotFound) {
				l.Audit.recordAs(ctx, user, "", AuditLockedOut, "user", string(user.ID), nil)
			}
			return SignedIn{}, &AccountLockedError{RetryAfter: LockoutDuration}
		}
		return SignedIn{}, ErrInvalidCredentials
	}
	l.Throttle.succeededLogIn(ctx, email)

	// The session binds to the oldest membership; switching
	// organisation is explicit.
	memberships, err := l.Accounts.MembershipsOf(ctx, user.ID)
	if err != nil {
		return SignedIn{}, err
	}
	if len(memberships) == 0 {
		return SignedIn{}, ErrNoOrganisation
	}
	m := memberships[0]
	session, err := l.Issuer.issue(ctx, user, m.Organisation.ID)
	if err != nil {
		return SignedIn{}, err
	}
	l.Audit.recordAs(ctx, user, m.Organisation.ID, AuditLoggedIn, "user", string(user.ID), nil)
	return SignedIn{User: user, Organisation: m.Organisation, Role: m.Role, Session: session}, nil
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
	Cache    *Cache
}

func (l *LogOut) Run(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	l.Cache.ForgetSession(ctx, token)
	return l.Sessions.Revoke(ctx, HashSecret(token), l.Clock.Now())
}

// UserPrincipal is a signed-in dashboard user, the organisation their
// session is bound to, and their role there.
type UserPrincipal struct {
	User         User
	Organisation Organisation
	Role         domain.Role
	SessionID    string
	ExpiresAt    time.Time
}

// Can reports whether the principal's role has a permission.
func (p UserPrincipal) Can(perm domain.Permission) bool { return p.Role.Can(perm) }

// ResolveSession turns a session token into the signed-in user. A
// session whose membership is gone (the member was removed) no longer
// resolves. Resolved sessions are cached briefly; every change to
// access invalidates them. A cached principal has no password hash.
type ResolveSession struct {
	Sessions SessionStore
	Accounts AccountStore
	Clock    Clock
	Cache    *Cache
}

func (r *ResolveSession) Run(ctx context.Context, token string) (UserPrincipal, error) {
	if token == "" {
		return UserPrincipal{}, ErrUnauthenticated
	}
	hash, now := HashSecret(token), r.Clock.Now()
	if p, ok := r.Cache.session(ctx, hash, now); ok {
		return p, nil
	}
	p, err := r.resolve(ctx, hash)
	if err != nil {
		return UserPrincipal{}, err
	}
	r.Cache.putSession(ctx, hash, p, now)
	p.User.PasswordHash = ""
	return p, nil
}

func (r *ResolveSession) resolve(ctx context.Context, hash string) (UserPrincipal, error) {
	s, err := r.Sessions.Get(ctx, hash)
	if errors.Is(err, ErrNotFound) {
		return UserPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return UserPrincipal{}, err
	}
	if s.RevokedAt != nil || !r.Clock.Now().Before(s.ExpiresAt) {
		return UserPrincipal{}, ErrUnauthenticated
	}
	user, err := r.Accounts.UserByID(ctx, s.UserID)
	if err != nil {
		return UserPrincipal{}, unauthenticatedIfMissing(err)
	}
	m, err := r.Accounts.Membership(ctx, s.OrgID, s.UserID)
	if err != nil {
		return UserPrincipal{}, unauthenticatedIfMissing(err)
	}
	return UserPrincipal{User: user, Organisation: m.Organisation, Role: m.Role, SessionID: s.ID, ExpiresAt: s.ExpiresAt}, nil
}

func unauthenticatedIfMissing(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrUnauthenticated
	}
	return err
}

// SwitchOrganisation moves the signed-in user to another organisation
// they belong to: a new session bound to it replaces the current one.
type SwitchOrganisation struct {
	Accounts AccountStore
	Sessions SessionStore
	Issuer   *SessionIssuer
	Cache    *Cache
}

func (s *SwitchOrganisation) Run(ctx context.Context, p UserPrincipal, currentToken string, to domain.OrgID) (SignedIn, error) {
	m, err := s.Accounts.Membership(ctx, to, p.User.ID)
	if err != nil {
		return SignedIn{}, err // ErrNotFound: not a member, or no such organisation
	}
	session, err := s.Issuer.issue(ctx, p.User, to)
	if err != nil {
		return SignedIn{}, err
	}
	s.Cache.ForgetSession(ctx, currentToken)
	if err := s.Sessions.Revoke(ctx, HashSecret(currentToken), s.Issuer.Clock.Now()); err != nil {
		return SignedIn{}, err
	}
	return SignedIn{User: p.User, Organisation: m.Organisation, Role: m.Role, Session: session}, nil
}

// ListMyOrganisations lists the organisations a user belongs to.
type ListMyOrganisations struct{ Accounts AccountStore }

func (l *ListMyOrganisations) Run(ctx context.Context, user domain.UserID) ([]Membership, error) {
	return l.Accounts.MembershipsOf(ctx, user)
}
