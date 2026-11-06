package sandbox

import (
	"context"
	"errors"
	"fmt"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// demoEmail is the fixed owner email the demo organisation is found or
// created by. Nothing can sign in with it: its password hash is
// app.DummyPasswordHash, which no real password verifies against.
const demoEmail = domain.Email("demo@enquoir.local")

// demoOrgName is the demo organisation's display name.
const demoOrgName = "Enquoir Demo"

// EnsureDemoOrg returns the demo organisation's ID, creating it only
// when a lookup by its fixed owner email confirms it is absent. Unlike
// a real sign-up this is not meant for a person: it exists so setup,
// demo and chaos tooling has a real organisation to submit intents
// under, without needing its own sign-up flow.
func EnsureDemoOrg(ctx context.Context, accounts app.AccountStore, ids app.IDGenerator, clock app.Clock) (domain.OrgID, error) {
	if user, err := accounts.UserByEmail(ctx, demoEmail); err == nil {
		return ownedOrg(ctx, accounts, user.ID)
	} else if !errors.Is(err, app.ErrNotFound) {
		return "", fmt.Errorf("looking up the demo organisation: %w", err)
	}

	now := clock.Now()
	org := app.Organisation{ID: domain.OrgID(ids.NewID("org")), Name: demoOrgName, CreatedAt: now}
	owner := app.User{
		ID:    domain.UserID(ids.NewID("usr")),
		Email: demoEmail, PasswordHash: app.DummyPasswordHash, CreatedAt: now,
	}

	err := accounts.CreateOrganisationWithOwner(ctx, org, owner)
	if err == nil {
		return org.ID, nil
	}
	if !errors.Is(err, app.ErrEmailTaken) {
		return "", fmt.Errorf("creating the demo organisation: %w", err)
	}

	// Lost a race to create it: whoever won is the real answer.
	user, err := accounts.UserByEmail(ctx, demoEmail)
	if err != nil {
		return "", fmt.Errorf("looking up the demo organisation after losing the race to create it: %w", err)
	}
	return ownedOrg(ctx, accounts, user.ID)
}

// ownedOrg is the demo owner's organisation: their first membership.
func ownedOrg(ctx context.Context, accounts app.AccountStore, user domain.UserID) (domain.OrgID, error) {
	ms, err := accounts.MembershipsOf(ctx, user)
	if err != nil {
		return "", fmt.Errorf("looking up the demo organisation: %w", err)
	}
	if len(ms) == 0 {
		return "", errors.New("the demo owner belongs to no organisation")
	}
	return ms[0].Organisation.ID, nil
}
