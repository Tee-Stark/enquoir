package app_test

import (
	"context"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type stubRegistrar struct{ id string }

func (r stubRegistrar) Register(ctx context.Context, b domain.Beneficiary) (string, error) {
	return r.id, nil
}

func TestRegisterBeneficiary_RecordsTheOrganisationAsOwner(t *testing.T) {
	b, err := domain.NewBeneficiary(testOrg, "Acme Ltd", "50001121", "021000021", domain.Checking, domain.BeneficiaryAddress{
		Street: "412 5th Avenue", City: "Seattle", State: "WA", Postcode: "98104",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, clock := apptest.NewBeneficiaries(), apptest.NewClock()
	use := &app.RegisterBeneficiary{Registrar: stubRegistrar{id: "ben_rail_1"}, Beneficiaries: store, Clock: clock}

	got, err := use.Run(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ben_rail_1" || got.Key != b.Key {
		t.Errorf("result = %+v", got)
	}
	if owns, _ := store.Owns(context.Background(), testOrg, "ben_rail_1"); !owns {
		t.Error("the registering organisation does not own the payee")
	}
	saved, _ := store.Get(b.Key)
	if saved.AccountLast4 != "1121" || saved.Name != "Acme Ltd" || !saved.CreatedAt.Equal(clock.Now()) {
		t.Errorf("saved = %+v, want the name, last four digits and registration time", saved)
	}
}
