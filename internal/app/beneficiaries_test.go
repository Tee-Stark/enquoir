package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type stubRegistrar struct {
	id    string
	calls *int
}

func (r stubRegistrar) Register(ctx context.Context, b domain.Beneficiary) (string, error) {
	if r.calls != nil {
		*r.calls++
	}
	return r.id, nil
}

var acmeAddress = domain.BeneficiaryAddress{Street: "412 5th Avenue", City: "Seattle", State: "WA", Postcode: "98104"}

func acmeCommand(org domain.OrgID) app.RegisterBeneficiaryCommand {
	return app.RegisterBeneficiaryCommand{OrgID: org, Name: "Acme Ltd", AccountNumber: "50001121", RoutingNumber: "021000021", Category: "Checking", Address: acmeAddress}
}

func TestRegisterBeneficiary_RecordsTheOrganisationAndTreasuryAsOwner(t *testing.T) {
	store, clock := apptest.NewBeneficiaries(), apptest.NewClock()
	use := &app.RegisterBeneficiary{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Registrar: stubRegistrar{id: "ben_rail_1"}, Beneficiaries: store, Clock: clock}

	got, err := use.Run(context.Background(), acmeCommand(testOrg))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := domain.NewBeneficiary(testOrg, "trs_test", "Acme Ltd", "50001121", "021000021", domain.Checking, acmeAddress)
	if got.ID != "ben_rail_1" || got.Key != want.Key {
		t.Errorf("result = %+v, want the key with the treasury %q", got, want.Key)
	}
	if owns, _ := store.Owns(context.Background(), testOrg, "trs_test", "ben_rail_1"); !owns {
		t.Error("the registering treasury does not own the payee")
	}
	if owns, _ := store.Owns(context.Background(), testOrg, "trs_other", "ben_rail_1"); owns {
		t.Error("another treasury owns the payee")
	}
	saved, _ := store.Get(want.Key)
	if saved.AccountLast4 != "1121" || saved.Name != "Acme Ltd" || saved.TreasuryID != "trs_test" || !saved.CreatedAt.Equal(clock.Now()) {
		t.Errorf("saved = %+v, want the treasury, name, last four digits and registration time", saved)
	}
}

func TestRegisterBeneficiary_ValidatesTheDetails(t *testing.T) {
	use := &app.RegisterBeneficiary{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Registrar: stubRegistrar{id: "x"}, Beneficiaries: apptest.NewBeneficiaries(), Clock: apptest.NewClock()}
	cmd := acmeCommand(testOrg)
	cmd.RoutingNumber = "123"
	var verr *app.ValidationError
	if _, err := use.Run(context.Background(), cmd); !errors.As(err, &verr) {
		t.Errorf("bad routing number: %v, want a validation error", err)
	}
}

// A payee registered before treasuries keeps its key - and its rail
// nickname - without one. Registering it again finds it: a second rail
// beneficiary under a new nickname would be a second payee ID, and the
// same invoice paid to it a second payment.
func TestRegisterBeneficiary_FindsAPayeeRegisteredBeforeTreasuries(t *testing.T) {
	store := apptest.NewBeneficiaries()
	legacy, _ := domain.NewBeneficiary(testOrg, "trs_test", "Acme Ltd", "50001121", "021000021", domain.Checking, acmeAddress)
	store.Seed(app.RegisteredBeneficiary{OrgID: testOrg, TreasuryID: "trs_test", Key: legacy.LegacyKey(), RailID: "ben_old"})
	calls := 0
	use := &app.RegisterBeneficiary{Treasuries: apptest.FixedTreasury{ID: "trs_test"}, Registrar: stubRegistrar{id: "ben_new", calls: &calls}, Beneficiaries: store, Clock: apptest.NewClock()}

	got, err := use.Run(context.Background(), acmeCommand(testOrg))
	if err != nil || got.ID != "ben_old" || got.Key != legacy.LegacyKey() || calls != 0 {
		t.Errorf("result = %+v, %v, %d rail registrations; want the existing payee and none", got, err, calls)
	}
	// Another organisation's identical details are its own payee.
	if got, _ := use.Run(context.Background(), acmeCommand("org_other")); got.ID != "ben_new" || calls != 1 {
		t.Errorf("another organisation: %+v, %d registrations", got, calls)
	}
}
