//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func TestBeneficiaryStore_OwnershipIsPerOrganisation(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	accounts, store := NewAccountStore(pool), NewBeneficiaryStore(pool)
	orgA, _ := newTestOrg(t, accounts)
	orgB, _ := newTestOrg(t, accounts)

	b := app.RegisteredBeneficiary{
		OrgID: orgA.ID, TreasuryID: treasuryOf(orgA.ID), Key: domain.BeneficiaryKey(ids.NewID("bkey")), RailID: ids.NewID("rail"),
		Name: "Acme Ltd", AccountLast4: "1121", RoutingNumber: "021000021", Category: domain.Checking, CreatedAt: time.Now(),
	}
	for i := 0; i < 2; i++ {
		if err := store.Save(ctx, b); err != nil {
			t.Fatalf("save #%d: %v", i+1, err)
		}
	}

	for _, tc := range []struct {
		org    domain.OrgID
		railID string
		want   bool
	}{
		{orgA.ID, b.RailID, true},
		{orgB.ID, b.RailID, false},
		{orgA.ID, "unknown", false},
	} {
		got, err := store.Owns(ctx, tc.org, treasuryOf(tc.org), tc.railID)
		if err != nil || got != tc.want {
			t.Errorf("Owns(%s, %s) = %v, %v; want %v", tc.org, tc.railID, got, err, tc.want)
		}
	}

	// A rail beneficiary can never be handed to a second organisation.
	stolen := b
	stolen.OrgID, stolen.TreasuryID, stolen.Key = orgB.ID, treasuryOf(orgB.ID), domain.BeneficiaryKey(ids.NewID("bkey"))
	if err := store.Save(ctx, stolen); err == nil {
		t.Error("saving another organisation's rail beneficiary: want an error")
	}
	if owns, _ := store.Owns(ctx, orgB.ID, treasuryOf(orgB.ID), b.RailID); owns {
		t.Error("org B owns org A's rail beneficiary")
	}
	if owns, _ := store.Owns(ctx, orgA.ID, "trs_another", b.RailID); owns {
		t.Error("another treasury owns org A's payee")
	}
	got, err := store.ByKey(ctx, orgA.ID, treasuryOf(orgA.ID), b.Key)
	if err != nil || got.RailID != b.RailID {
		t.Errorf("ByKey: %+v, %v", got, err)
	}
	if _, err := store.ByKey(ctx, orgB.ID, treasuryOf(orgB.ID), b.Key); err == nil {
		t.Error("ByKey across organisations found the payee")
	}
}

// The schema keeps a payment in a treasury of its own organisation.
func TestTreasuries_CreatedWithTheOrganisationAndEnforced(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	accounts := NewAccountStore(pool)
	org, _ := newTestOrg(t, accounts)
	other, _ := newTestOrg(t, accounts)
	tr, err := accounts.TreasuryOf(ctx, org.ID)
	if err != nil || tr.ID != treasuryOf(org.ID) || tr.Name != app.DefaultTreasuryName {
		t.Fatalf("treasury = %+v, %v", tr, err)
	}
	intent, _ := domain.NewIntent(org.ID, treasuryOf(other.ID), "payee", 100, "USD", "ref-"+ids.NewID("r"), "")
	if _, _, err := NewIntentRepository(pool).Save(ctx, intent); err == nil {
		t.Error("stored an intent in another organisation's treasury")
	}
	dup := app.Organisation{ID: domain.OrgID(ids.NewID("org")), Name: "No treasury", CreatedAt: time.Now()}
	if err := accounts.CreateOrganisationWithOwner(ctx, dup, app.User{ID: domain.UserID(ids.NewID("usr")), Email: domain.Email(ids.NewID("e") + "@example.com"), PasswordHash: "h", CreatedAt: time.Now()}); err == nil {
		t.Error("created an organisation without a treasury")
	}
}
