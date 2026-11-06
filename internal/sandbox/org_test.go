package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type fakeAccounts struct {
	byEmail map[domain.Email]app.User
	creates int
	// createErr is returned once, then cleared, simulating a racing
	// creator winning between our lookup and our create.
	createErr error
}

func (a *fakeAccounts) CreateOrganisationWithOwner(ctx context.Context, org app.Organisation, owner app.User) error {
	a.creates++
	if a.createErr != nil {
		err := a.createErr
		a.createErr = nil
		return err
	}
	if a.byEmail == nil {
		a.byEmail = map[domain.Email]app.User{}
	}
	a.byEmail[owner.Email] = owner
	return nil
}

func (a *fakeAccounts) UserByEmail(ctx context.Context, email domain.Email) (app.User, error) {
	u, ok := a.byEmail[email]
	if !ok {
		return app.User{}, app.ErrNotFound
	}
	return u, nil
}

func (a *fakeAccounts) UserWithOrganisation(ctx context.Context, id domain.UserID) (app.User, app.Organisation, error) {
	return app.User{}, app.Organisation{}, app.ErrNotFound
}

type fixedIDs struct{ n int }

func (g *fixedIDs) NewID(prefix string) string {
	g.n++
	return prefix + "_fixed"
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestEnsureDemoOrg_AbsentIsCreated(t *testing.T) {
	accounts := &fakeAccounts{}
	org, err := EnsureDemoOrg(context.Background(), accounts, &fixedIDs{}, fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("EnsureDemoOrg: unexpected error: %v", err)
	}
	if org == "" {
		t.Error("expected a non-empty org ID")
	}
	if accounts.creates != 1 {
		t.Errorf("creates = %d, want 1", accounts.creates)
	}
}

func TestEnsureDemoOrg_AlreadyExistsIsReused(t *testing.T) {
	accounts := &fakeAccounts{}
	first, err := EnsureDemoOrg(context.Background(), accounts, &fixedIDs{}, fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("first EnsureDemoOrg: unexpected error: %v", err)
	}

	second, err := EnsureDemoOrg(context.Background(), accounts, &fixedIDs{}, fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("second EnsureDemoOrg: unexpected error: %v", err)
	}
	if second != first {
		t.Errorf("second call returned a different org: %q vs %q", second, first)
	}
	if accounts.creates != 1 {
		t.Errorf("creates = %d, want 1 (the second call should never create)", accounts.creates)
	}
}

// Two commands racing to bootstrap the same demo org both resolve to
// the one that actually landed, rather than one of them failing.
func TestEnsureDemoOrg_RaceToCreateResolvesToTheWinner(t *testing.T) {
	accounts := &fakeAccounts{createErr: app.ErrEmailTaken}
	// Seed the "winner" as if a concurrent call already landed.
	accounts.byEmail = map[domain.Email]app.User{}
	winnerOrg := domain.OrgID("org_winner")
	accounts.byEmail[demoEmail] = app.User{OrgID: winnerOrg, Email: demoEmail}

	org, err := EnsureDemoOrg(context.Background(), accounts, &fixedIDs{}, fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("EnsureDemoOrg: unexpected error: %v", err)
	}
	if org != winnerOrg {
		t.Errorf("org = %q, want the winner's %q", org, winnerOrg)
	}
}

func TestEnsureDemoOrg_PropagatesUnexpectedLookupError(t *testing.T) {
	wantErr := errors.New("postgres unavailable")
	accounts := &failingAccounts{err: wantErr}
	_, err := EnsureDemoOrg(context.Background(), accounts, &fixedIDs{}, fixedClock{t: time.Now()})
	if !errors.Is(err, wantErr) {
		t.Errorf("EnsureDemoOrg error = %v, want it to wrap %v", err, wantErr)
	}
}

type failingAccounts struct{ err error }

func (a *failingAccounts) CreateOrganisationWithOwner(ctx context.Context, org app.Organisation, owner app.User) error {
	return a.err
}

func (a *failingAccounts) UserByEmail(ctx context.Context, email domain.Email) (app.User, error) {
	return app.User{}, a.err
}

func (a *failingAccounts) UserWithOrganisation(ctx context.Context, id domain.UserID) (app.User, app.Organisation, error) {
	return app.User{}, app.Organisation{}, a.err
}
