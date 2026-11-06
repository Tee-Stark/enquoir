package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type accountsFixture struct {
	accounts *apptest.Accounts
	sessions *apptest.Sessions
	hasher   *apptest.Hasher
	clock    *apptest.Clock
	signUp   *app.SignUp
	logIn    *app.LogIn
	logOut   *app.LogOut
	resolve  *app.ResolveSession
	issuer   *app.SessionIssuer
	emails   *app.Emails
	tokens   *apptest.Tokens
}

func newAccountsFixture() *accountsFixture {
	f := &accountsFixture{
		accounts: apptest.NewAccounts(),
		sessions: apptest.NewSessions(),
		hasher:   &apptest.Hasher{},
		clock:    apptest.NewClock(),
	}
	issuer := &app.SessionIssuer{Sessions: f.sessions, IDs: &apptest.IDs{}, Secrets: &apptest.Secrets{}, Clock: f.clock, TTL: 7 * 24 * time.Hour}
	f.signUp = &app.SignUp{Accounts: f.accounts, Hasher: f.hasher, IDs: &apptest.IDs{}, Clock: f.clock, Issuer: issuer}
	f.logIn = &app.LogIn{Accounts: f.accounts, Hasher: f.hasher, Issuer: issuer}
	f.logOut = &app.LogOut{Sessions: f.sessions, Clock: f.clock}
	f.issuer = issuer
	f.resolve = &app.ResolveSession{Sessions: f.sessions, Accounts: f.accounts, Clock: f.clock}
	return f
}

var validSignUp = app.SignUpCommand{Email: " Ada@Example.com ", Password: "correct horse battery", OrganisationName: "Acme Treasury"}

func TestSignUp_CreatesOrganisationOwnerAndSession(t *testing.T) {
	f := newAccountsFixture()
	got, err := f.signUp.Run(context.Background(), validSignUp)
	if err != nil {
		t.Fatalf("app.SignUp: %v", err)
	}
	if got.User.Email != "ada@example.com" || got.User.EmailVerified {
		t.Errorf("user = %+v, want normalised email, not yet verified", got.User)
	}
	if got.Organisation.Name != "Acme Treasury" || got.Role != domain.Owner {
		t.Errorf("organisation = %+v, role %q: the signer-up must own the new organisation", got.Organisation, got.Role)
	}
	if m, err := f.accounts.Membership(context.Background(), got.Organisation.ID, got.User.ID); err != nil || m.Role != domain.Owner {
		t.Errorf("membership = %+v, %v; want an owner membership", m, err)
	}
	if got.User.PasswordHash == validSignUp.Password {
		t.Error("the password was stored in plaintext")
	}
	if got.Session.Token == "" || !got.Session.ExpiresAt.Equal(f.clock.Now().Add(7*24*time.Hour)) {
		t.Errorf("session = %+v, want a token expiring in 7 days", got.Session)
	}
	if f.sessions.StoresPlaintext(got.Session.Token) {
		t.Error("the session token was stored in plaintext; only its hash may be")
	}
}

func TestSignUp_RejectsInvalidInputWithTheField(t *testing.T) {
	cases := map[string]struct {
		cmd   app.SignUpCommand
		field string
	}{
		"bad email":      {app.SignUpCommand{Email: "nope", Password: "correct horse battery", OrganisationName: "Acme"}, "email"},
		"short password": {app.SignUpCommand{Email: "a@b.co", Password: "short", OrganisationName: "Acme"}, "password"},
		"no org name":    {app.SignUpCommand{Email: "a@b.co", Password: "correct horse battery", OrganisationName: " "}, "organisation_name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newAccountsFixture().signUp.Run(context.Background(), tc.cmd)
			var verr *app.ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Errorf("err = %v, want a app.ValidationError on %q", err, tc.field)
			}
		})
	}
}

func TestSignUp_RejectsAnEmailAlreadyRegistered(t *testing.T) {
	f := newAccountsFixture()
	if _, err := f.signUp.Run(context.Background(), validSignUp); err != nil {
		t.Fatal(err)
	}
	again := validSignUp
	again.Email = "ADA@example.com"
	if _, err := f.signUp.Run(context.Background(), again); !errors.Is(err, app.ErrEmailTaken) {
		t.Errorf("err = %v, want app.ErrEmailTaken (emails compare case-insensitively)", err)
	}
}

func TestLogIn_ReturnsANewSessionForTheRightPassword(t *testing.T) {
	f := newAccountsFixture()
	signed, _ := f.signUp.Run(context.Background(), validSignUp)

	got, err := f.logIn.Run(context.Background(), "ada@EXAMPLE.com", validSignUp.Password)
	if err != nil {
		t.Fatalf("app.LogIn: %v", err)
	}
	if got.User.ID != signed.User.ID || got.Session.Token == signed.Session.Token {
		t.Errorf("login = %+v, want the same user with a fresh session", got)
	}
}

// Unknown email and wrong password are indistinguishable to the
// caller, and both run a password verification, so neither the answer
// nor its timing reveals which emails have accounts.
func TestLogIn_FailsTheSameWayForUnknownEmailAndWrongPassword(t *testing.T) {
	f := newAccountsFixture()
	f.signUp.Run(context.Background(), validSignUp)

	for _, tc := range []struct{ email, pw string }{
		{"ada@example.com", "wrong password here"},
		{"nobody@example.com", validSignUp.Password},
		{"not an email", validSignUp.Password},
	} {
		before := f.hasher.VerifyCalls
		_, err := f.logIn.Run(context.Background(), tc.email, tc.pw)
		if !errors.Is(err, app.ErrInvalidCredentials) {
			t.Errorf("app.LogIn(%q): err = %v, want app.ErrInvalidCredentials", tc.email, err)
		}
		if f.hasher.VerifyCalls != before+1 {
			t.Errorf("app.LogIn(%q): %d password verifications, want exactly 1", tc.email, f.hasher.VerifyCalls-before)
		}
	}
}

func TestResolveSession(t *testing.T) {
	f := newAccountsFixture()
	signed, _ := f.signUp.Run(context.Background(), validSignUp)
	ctx := context.Background()

	p, err := f.resolve.Run(ctx, signed.Session.Token)
	if err != nil || p.User.ID != signed.User.ID || p.Organisation.ID != signed.Organisation.ID {
		t.Fatalf("Resolve(valid) = %+v, %v", p, err)
	}

	if _, err := f.resolve.Run(ctx, "ses_made_up"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("Resolve(unknown) err = %v, want app.ErrUnauthenticated", err)
	}

	f.clock.Advance(7*24*time.Hour + time.Second)
	if _, err := f.resolve.Run(ctx, signed.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("Resolve(expired) err = %v, want app.ErrUnauthenticated", err)
	}
}

func TestLogOut_RevokesTheSessionAndIsIdempotent(t *testing.T) {
	f := newAccountsFixture()
	signed, _ := f.signUp.Run(context.Background(), validSignUp)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := f.logOut.Run(ctx, signed.Session.Token); err != nil {
			t.Fatalf("app.LogOut #%d: %v", i+1, err)
		}
	}
	if _, err := f.resolve.Run(ctx, signed.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("Resolve after logout err = %v, want app.ErrUnauthenticated", err)
	}
}

func TestLogIn_BindsTheSessionToTheOldestMembershipAndSwitches(t *testing.T) {
	f := newAccountsFixture()
	ctx := context.Background()
	ada, _ := f.signUp.Run(ctx, validSignUp)
	bobs, _ := f.signUp.Run(ctx, app.SignUpCommand{Email: "bob@example.com", Password: "correct horse battery", OrganisationName: "Bob Co"})
	f.clock.Advance(time.Minute)
	f.accounts.AddMember(bobs.Organisation.ID, ada.User.ID, domain.Viewer, f.clock.Now())

	in, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password)
	if err != nil || in.Organisation.ID != ada.Organisation.ID || in.Role != domain.Owner {
		t.Fatalf("login = %+v, %v; want ada's own (oldest) organisation as owner", in, err)
	}

	p, _ := f.resolve.Run(ctx, in.Session.Token)
	switcher := &app.SwitchOrganisation{Accounts: f.accounts, Sessions: f.sessions, Issuer: f.issuer}
	switched, err := switcher.Run(ctx, p, in.Session.Token, bobs.Organisation.ID)
	if err != nil || switched.Role != domain.Viewer || switched.Organisation.Name != "Bob Co" {
		t.Fatalf("switch = %+v, %v; want Bob Co as viewer", switched, err)
	}
	if _, err := f.resolve.Run(ctx, in.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("the session switched away from still resolves: %v", err)
	}
	if p2, err := f.resolve.Run(ctx, switched.Session.Token); err != nil || p2.Organisation.ID != bobs.Organisation.ID || p2.Role != domain.Viewer {
		t.Errorf("the new session = %+v, %v", p2, err)
	}

	stranger, _ := f.signUp.Run(ctx, app.SignUpCommand{Email: "eve@example.com", Password: "correct horse battery", OrganisationName: "Eve"})
	if _, err := switcher.Run(ctx, p, in.Session.Token, stranger.Organisation.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("switching to an organisation ada is not in: %v, want ErrNotFound", err)
	}
}

func TestResolveSession_StopsWhenTheMembershipIsGone(t *testing.T) {
	f := newAccountsFixture()
	ctx := context.Background()
	signed, _ := f.signUp.Run(ctx, validSignUp)
	f.accounts.RemoveMembershipForTest(signed.Organisation.ID, signed.User.ID)
	if _, err := f.resolve.Run(ctx, signed.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("a removed member's session resolves: %v", err)
	}
	if _, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password); !errors.Is(err, app.ErrNoOrganisation) {
		t.Errorf("logging in with no organisation: %v, want ErrNoOrganisation", err)
	}
}
