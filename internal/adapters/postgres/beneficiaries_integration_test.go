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
		OrgID: orgA.ID, Key: domain.BeneficiaryKey(ids.NewID("bkey")), RailID: ids.NewID("rail"),
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
		got, err := store.Owns(ctx, tc.org, tc.railID)
		if err != nil || got != tc.want {
			t.Errorf("Owns(%s, %s) = %v, %v; want %v", tc.org, tc.railID, got, err, tc.want)
		}
	}

	// A rail beneficiary can never be handed to a second organisation.
	stolen := b
	stolen.OrgID, stolen.Key = orgB.ID, domain.BeneficiaryKey(ids.NewID("bkey"))
	if err := store.Save(ctx, stolen); err == nil {
		t.Error("saving another organisation's rail beneficiary: want an error")
	}
	if owns, _ := store.Owns(ctx, orgB.ID, b.RailID); owns {
		t.Error("org B owns org A's rail beneficiary")
	}
}
