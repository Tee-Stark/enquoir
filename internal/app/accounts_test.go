package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

type accountsFixture struct {
	accounts *fakeAccounts
	sessions *fakeSessions
	hasher   *fakeHasher
	clock    *fixedClock
	signUp   *SignUp
	logIn    *LogIn
	logOut   *LogOut
	resolve  *ResolveSession
}

func newAccountsFixture() *accountsFixture {
	f := &accountsFixture{
		accounts: newFakeAccounts(),
		sessions: newFakeSessions(),
		hasher:   &fakeHasher{},
		clock:    newFixedClock(),
	}
	issuer := &SessionIssuer{Sessions: f.sessions, Secrets: &seqSecrets{}, Clock: f.clock, TTL: 7 * 24 * time.Hour}
	f.signUp = &SignUp{Accounts: f.accounts, Hasher: f.hasher, IDs: &seqIDs{}, Clock: f.clock, Issuer: issuer}
	f.logIn = &LogIn{Accounts: f.accounts, Hasher: f.hasher, Issuer: issuer}
	f.logOut = &LogOut{Sessions: f.sessions, Clock: f.clock}
	f.resolve = &ResolveSession{Sessions: f.sessions, Accounts: f.accounts, Clock: f.clock}
	return f
}

var validSignUp = SignUpCommand{Email: " Ada@Example.com ", Password: "correct horse battery", OrganisationName: "Acme Treasury"}

func TestSignUp_CreatesOrganisationOwnerAndSession(t *testing.T) {
	f := newAccountsFixture()
	got, err := f.signUp.Run(context.Background(), validSignUp)
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	if got.User.Email != "ada@example.com" || got.User.EmailVerified {
		t.Errorf("user = %+v, want normalised email, not yet verified", got.User)
	}
	if got.Organisation.Name != "Acme Treasury" || got.User.OrgID != got.Organisation.ID {
		t.Errorf("organisation = %+v, user org %q: the owner must belong to the new organisation", got.Organisation, got.User.OrgID)
	}
	if got.User.PasswordHash == validSignUp.Password {
		t.Error("the password was stored in plaintext")
	}
	if got.Session.Token == "" || !got.Session.ExpiresAt.Equal(f.clock.Now().Add(7*24*time.Hour)) {
		t.Errorf("session = %+v, want a token expiring in 7 days", got.Session)
	}
	if f.sessions.storesPlaintext(got.Session.Token) {
		t.Error("the session token was stored in plaintext; only its hash may be")
	}
}

func TestSignUp_RejectsInvalidInputWithTheField(t *testing.T) {
	cases := map[string]struct {
		cmd   SignUpCommand
		field string
	}{
		"bad email":      {SignUpCommand{Email: "nope", Password: "correct horse battery", OrganisationName: "Acme"}, "email"},
		"short password": {SignUpCommand{Email: "a@b.co", Password: "short", OrganisationName: "Acme"}, "password"},
		"no org name":    {SignUpCommand{Email: "a@b.co", Password: "correct horse battery", OrganisationName: " "}, "organisation_name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newAccountsFixture().signUp.Run(context.Background(), tc.cmd)
			var verr *ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Errorf("err = %v, want a ValidationError on %q", err, tc.field)
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
	if _, err := f.signUp.Run(context.Background(), again); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("err = %v, want ErrEmailTaken (emails compare case-insensitively)", err)
	}
}

func TestLogIn_ReturnsANewSessionForTheRightPassword(t *testing.T) {
	f := newAccountsFixture()
	signed, _ := f.signUp.Run(context.Background(), validSignUp)

	got, err := f.logIn.Run(context.Background(), "ada@EXAMPLE.com", validSignUp.Password)
	if err != nil {
		t.Fatalf("LogIn: %v", err)
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
		before := f.hasher.verifyCalls
		_, err := f.logIn.Run(context.Background(), tc.email, tc.pw)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("LogIn(%q): err = %v, want ErrInvalidCredentials", tc.email, err)
		}
		if f.hasher.verifyCalls != before+1 {
			t.Errorf("LogIn(%q): %d password verifications, want exactly 1", tc.email, f.hasher.verifyCalls-before)
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

	if _, err := f.resolve.Run(ctx, "ses_made_up"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Resolve(unknown) err = %v, want ErrUnauthenticated", err)
	}

	f.clock.Advance(7*24*time.Hour + time.Second)
	if _, err := f.resolve.Run(ctx, signed.Session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Resolve(expired) err = %v, want ErrUnauthenticated", err)
	}
}

func TestLogOut_RevokesTheSessionAndIsIdempotent(t *testing.T) {
	f := newAccountsFixture()
	signed, _ := f.signUp.Run(context.Background(), validSignUp)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := f.logOut.Run(ctx, signed.Session.Token); err != nil {
			t.Fatalf("LogOut #%d: %v", i+1, err)
		}
	}
	if _, err := f.resolve.Run(ctx, signed.Session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Resolve after logout err = %v, want ErrUnauthenticated", err)
	}
}
