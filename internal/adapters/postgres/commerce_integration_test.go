//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

type commerceUseCases struct {
	create *commerceapp.CreateIntegration
	status *commerceapp.SetIntegrationStatus
	rotate *commerceapp.RotateIntegrationKey
	revoke *commerceapp.RevokeIntegrationKey
	auth   *commerceapp.AuthenticateMerchant

	registerCustomer *commerceapp.RegisterCustomer
	customerStatus   *commerceapp.SetCustomerStatus
	listCustomers    *commerceapp.ListCustomers
	registerAgent    *commerceapp.RegisterAgent
	agentStatus      *commerceapp.SetAgentStatus
}

func newCommerceUseCases(t *testing.T) (*commerceUseCases, *MerchantIntegrationStore, *AuditLog) {
	pool := testPool(t)
	store, customers, agents := NewMerchantIntegrationStore(pool), NewCommerceCustomerStore(pool), NewCommerceAgentStore(pool)
	log := NewAuditLog(pool)
	clock, ids, secrets := realClock{}, credentials.RandomIDs{}, credentials.RandomSecrets{}
	audit := &app.Audit{Log: log, IDs: ids, Clock: clock}
	return &commerceUseCases{
		create: &commerceapp.CreateIntegration{Integrations: store, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		status: &commerceapp.SetIntegrationStatus{Integrations: store, Clock: clock, Audit: audit},
		rotate: &commerceapp.RotateIntegrationKey{Integrations: store, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		revoke: &commerceapp.RevokeIntegrationKey{Integrations: store, Clock: clock, Audit: audit},
		auth:   &commerceapp.AuthenticateMerchant{Integrations: store, Clock: clock},

		registerCustomer: &commerceapp.RegisterCustomer{Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		customerStatus:   &commerceapp.SetCustomerStatus{Customers: customers, Clock: clock, Audit: audit},
		listCustomers:    &commerceapp.ListCustomers{Customers: customers},
		registerAgent:    &commerceapp.RegisterAgent{Agents: agents, IDs: ids, Clock: clock, Audit: audit},
		agentStatus:      &commerceapp.SetAgentStatus{Agents: agents, Clock: clock, Audit: audit},
	}, store, log
}

// newMerchant connects a merchant for a fresh organisation and returns
// its principal and key.
func (u *commerceUseCases) newMerchant(t *testing.T, accounts *AccountStore) (commerceapp.MerchantPrincipal, string) {
	t.Helper()
	org, _ := newTestOrg(t, accounts)
	return u.newMerchantIn(t, org.ID)
}

func (u *commerceUseCases) newMerchantIn(t *testing.T, org domain.OrgID) (commerceapp.MerchantPrincipal, string) {
	t.Helper()
	issued, err := u.create.Run(context.Background(), org, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.auth.Run(context.Background(), issued.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return p, issued.Secret
}

func TestMerchantIntegrationStore_KeysStatusAndScoping(t *testing.T) {
	ctx := context.Background()
	u, store, _ := newCommerceUseCases(t)
	accounts := NewAccountStore(testPool(t))
	orgA, _ := newTestOrg(t, accounts)
	orgB, _ := newTestOrg(t, accounts)

	issued, err := u.create.Run(ctx, orgA.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	id := issued.Integration.ID
	p, err := u.auth.Run(ctx, issued.Secret)
	if err != nil || p.MerchantID != id || p.OrgID != orgA.ID || p.MerchantName != "Acme" {
		t.Fatalf("auth = %+v, %v", p, err)
	}
	got, _ := store.GetIntegration(ctx, orgA.ID, id)
	if got.Key == nil || got.Key.LastUsedAt == nil || got.Key.Prefix != issued.Secret[:12] || got.Status != commerce.Active {
		t.Errorf("after auth = %+v key %+v", got.Merchant, got.Key)
	}

	// Another organisation can neither see nor change it.
	if _, err := store.GetIntegration(ctx, orgB.ID, id); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("get across orgs: %v", err)
	}
	if list, _ := store.ListIntegrations(ctx, orgB.ID); len(list) != 0 {
		t.Errorf("org B lists %d integrations", len(list))
	}
	if _, err := u.status.Run(ctx, orgB.ID, id, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("disable across orgs: %v", err)
	}
	if _, err := u.rotate.Run(ctx, orgB.ID, id); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("rotate across orgs: %v", err)
	}
	if err := u.revoke.Run(ctx, orgB.ID, id); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoke across orgs: %v", err)
	}

	if _, err := u.auth.Run(ctx, issued.Secret); err != nil {
		t.Errorf("the key after another organisation's attempts: %v; want it untouched", err)
	}

	// Disabled, the key is refused; enabled, it works again.
	if got, err := u.status.Run(ctx, orgA.ID, id, commerce.Disabled); err != nil || got.DisabledAt == nil {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	if _, err := u.auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("disabled integration's key: %v", err)
	}
	if got, err := u.status.Run(ctx, orgA.ID, id, commerce.Active); err != nil || got.DisabledAt != nil {
		t.Fatalf("enable: %+v, %v", got, err)
	}
	if _, err := u.auth.Run(ctx, issued.Secret); err != nil {
		t.Errorf("re-enabled integration's key: %v", err)
	}

	rotated, err := u.rotate.Run(ctx, orgA.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("old key after rotation: %v", err)
	}
	if err := u.revoke.Run(ctx, orgA.ID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := u.auth.Run(ctx, rotated.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("revoked key: %v", err)
	}
	if got, _ := store.GetIntegration(ctx, orgA.ID, id); got.Key != nil {
		t.Errorf("after revoking, key = %+v", got.Key)
	}
	if revoked, err := store.RevokeKey(ctx, orgA.ID, id, realClock{}.Now()); err != nil || revoked {
		t.Errorf("revoking with no key: %v, %v; want nothing to revoke", revoked, err)
	}
}

// Concurrent rotations of one integration leave exactly one active key.
func TestMerchantIntegrationStore_ConcurrentRotationsLeaveOneKey(t *testing.T) {
	ctx := context.Background()
	u, store, _ := newCommerceUseCases(t)
	org, _ := newTestOrg(t, NewAccountStore(testPool(t)))
	issued, _ := u.create.Run(ctx, org.ID, "Acme")

	var wg sync.WaitGroup
	secrets := make([]string, 8)
	for i := range secrets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := u.rotate.Run(ctx, org.ID, issued.Integration.ID)
			if err != nil {
				t.Errorf("rotation %d: %v", i, err)
				return
			}
			secrets[i] = r.Secret
		}()
	}
	wg.Wait()
	working := 0
	for _, s := range append(secrets, issued.Secret) {
		if _, err := u.auth.Run(ctx, s); err == nil {
			working++
		}
	}
	got, _ := store.GetIntegration(ctx, org.ID, issued.Integration.ID)
	if working != 1 || got.Key == nil {
		t.Errorf("%d keys work after concurrent rotations, want exactly 1", working)
	}
}

func TestCommerceDirectory_RegisterOrResolveIsScopedToTheMerchant(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	accounts := NewAccountStore(testPool(t))
	orgA, _ := newTestOrg(t, accounts)
	orgB, _ := newTestOrg(t, accounts)
	a1, _ := u.newMerchantIn(t, orgA.ID)
	a2, _ := u.newMerchantIn(t, orgA.ID)
	b, _ := u.newMerchantIn(t, orgB.ID)

	first, created, err := u.registerCustomer.Run(ctx, a1, "cust_42", "Ada@Example.com")
	if err != nil || !created || first.Email != "ada@example.com" {
		t.Fatalf("register: %+v %v %v", first, created, err)
	}
	again, created, err := u.registerCustomer.Run(ctx, a1, "cust_42", "ada@example.com")
	if err != nil || created || again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("repeat: %+v %v %v; want the same record", again, created, err)
	}
	if _, _, err := u.registerCustomer.Run(ctx, a1, "cust_42", "eve@example.com"); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("different email: %v", err)
	}
	for _, other := range []commerceapp.MerchantPrincipal{a2, b} {
		c, created, err := u.registerCustomer.Run(ctx, other, "cust_42", "ada@example.com")
		if err != nil || !created || c.ID == first.ID {
			t.Errorf("same external ID under %s: %+v %v %v; want a separate record", other.MerchantID, c, created, err)
		}
		if _, err := u.customerStatus.Run(ctx, other, first.ID, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s disabling A1's customer: %v", other.MerchantID, err)
		}
	}
	// Not just reported as not found: actually untouched.
	if got, err := NewCommerceCustomerStore(testPool(t)).GetCustomer(ctx, a1.Scope(), first.ID); err != nil || got.Status != commerce.Active {
		t.Errorf("A1's customer after the others' attempts: %+v, %v; want it still active", got, err)
	}

	agent, created, err := u.registerAgent.Run(ctx, a1, "assistant", "Shopping assistant")
	if err != nil || !created {
		t.Fatal(err)
	}
	if got, err := u.agentStatus.Run(ctx, a1, agent.ID, commerce.Disabled); err != nil || got.Status != commerce.Disabled || got.DisabledAt == nil {
		t.Errorf("disable agent: %+v, %v", got, err)
	}
	if got, err := u.agentStatus.Run(ctx, a1, agent.ID, commerce.Disabled); err != nil || got.Status != commerce.Disabled {
		t.Errorf("disabling again: %+v, %v", got, err)
	}
	if _, err := u.agentStatus.Run(ctx, b, agent.ID, commerce.Active); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another org's merchant enabling the agent: %v", err)
	}
	if _, err := u.agentStatus.Run(ctx, a2, agent.ID, commerce.Active); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a sibling storefront enabling the agent: %v", err)
	}
	if got, _ := NewCommerceAgentStore(testPool(t)).GetAgent(ctx, a1.Scope(), agent.ID); got.Status != commerce.Disabled {
		t.Errorf("A1's agent after the others' attempts: %+v; want it still disabled", got)
	}
}

// Many concurrent registrations of one external ID make one record,
// and every caller gets it back.
func TestCommerceDirectory_ConcurrentRegistrationsMakeOneCustomer(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	m, _ := u.newMerchant(t, NewAccountStore(testPool(t)))

	var wg sync.WaitGroup
	ids := make([]commerce.CustomerID, 12)
	created := make([]bool, len(ids))
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, made, err := u.registerCustomer.Run(ctx, m, "cust_race", "race@example.com")
			if err != nil {
				t.Errorf("registration %d: %v", i, err)
				return
			}
			ids[i], created[i] = c.ID, made
		}()
	}
	wg.Wait()
	made := 0
	for i := range ids {
		if ids[i] != ids[0] {
			t.Errorf("registration %d got %s, want %s", i, ids[i], ids[0])
		}
		if created[i] {
			made++
		}
	}
	if made != 1 {
		t.Errorf("%d registrations report creating the customer, want 1", made)
	}
}

func TestCommerceDirectory_ListsPagesAndFilters(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	m, _ := u.newMerchant(t, NewAccountStore(testPool(t)))
	var made []commerce.Customer
	for i := range 5 {
		c, _, err := u.registerCustomer.Run(ctx, m, fmt.Sprintf("c%d", i), fmt.Sprintf("c%d@example.com", i))
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, c)
	}
	u.customerStatus.Run(ctx, m, made[2].ID, commerce.Disabled)

	var seen []commerce.CustomerID
	cursor := ""
	for range 5 {
		page, err := u.listCustomers.Run(ctx, m, commerceapp.ListFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Customers {
			seen = append(seen, c.ID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != 5 || seen[0] != made[4].ID || seen[4] != made[0].ID {
		t.Errorf("paged = %v, want all five newest first", seen)
	}
	byExt, _ := u.listCustomers.Run(ctx, m, commerceapp.ListFilter{ExternalID: "c3"})
	disabled, _ := u.listCustomers.Run(ctx, m, commerceapp.ListFilter{Status: commerce.Disabled})
	if len(byExt.Customers) != 1 || byExt.Customers[0].ID != made[3].ID || len(disabled.Customers) != 1 || disabled.Customers[0].ID != made[2].ID {
		t.Errorf("by external ID %+v; disabled %+v", byExt.Customers, disabled.Customers)
	}
	var verr *app.ValidationError
	if _, err := u.listCustomers.Run(ctx, m, commerceapp.ListFilter{Cursor: "not-a-cursor"}); !errors.As(err, &verr) {
		t.Errorf("bad cursor: %v, want a validation error", err)
	}
}

// The audit log names the merchant that acted with its key.
func TestAuditLog_NamesTheActingMerchant(t *testing.T) {
	u, _, log := newCommerceUseCases(t)
	m, _ := u.newMerchant(t, NewAccountStore(testPool(t)))
	ctx := app.WithActor(context.Background(), app.Actor{MerchantID: string(m.MerchantID), MerchantName: m.MerchantName, IP: "203.0.113.9"})
	c, _, err := u.registerCustomer.Run(ctx, m, "cust_audit", "audit@example.com")
	if err != nil {
		t.Fatal(err)
	}
	page, err := log.List(context.Background(), m.OrgID, app.AuditFilter{Action: app.AuditCommerceCustomerRegistered, Limit: 10})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("events = %+v, %v", page.Events, err)
	}
	e := page.Events[0]
	if e.ActorMerchant != string(m.MerchantID) || e.MerchantName != "Shop" || e.TargetID != string(c.ID) || e.Details["external_id"] != "cust_audit" {
		t.Errorf("event = %+v", e)
	}
}

type fundingUseCases struct {
	register *commerceapp.RegisterFundingMethod
	list     *commerceapp.ListFundingMethods
	status   *commerceapp.SetFundingMethodStatus
	store    *CommerceFundingMethodStore
}

func newFundingUseCases(t *testing.T) fundingUseCases {
	pool := testPool(t)
	store, customers := NewCommerceFundingMethodStore(pool), NewCommerceCustomerStore(pool)
	clock, ids := realClock{}, credentials.RandomIDs{}
	audit := &app.Audit{Log: NewAuditLog(pool), IDs: ids, Clock: clock}
	return fundingUseCases{
		register: &commerceapp.RegisterFundingMethod{Methods: store, Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		list:     &commerceapp.ListFundingMethods{Methods: store, Customers: customers},
		status:   &commerceapp.SetFundingMethodStatus{Methods: store, Clock: clock, Audit: audit},
		store:    store,
	}
}

func cardOf(customer commerce.CustomerID, ref string) commerceapp.FundingMethodCommand {
	return commerceapp.FundingMethodCommand{CustomerID: customer, Reference: ref, Provider: "stripe", Brand: "visa", LastFour: "4242"}
}

func TestCommerceFundingMethods_RegisterOrResolveListAndScope(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	fu := newFundingUseCases(t)
	accounts := NewAccountStore(testPool(t))
	orgA, _ := newTestOrg(t, accounts)
	orgB, _ := newTestOrg(t, accounts)
	a1, _ := u.newMerchantIn(t, orgA.ID)
	a2, _ := u.newMerchantIn(t, orgA.ID)
	b, _ := u.newMerchantIn(t, orgB.ID)
	ada, _, _ := u.registerCustomer.Run(ctx, a1, "ada", "ada@example.com")

	first, created, err := fu.register.Run(ctx, a1, cardOf(ada.ID, "mfm_1"))
	if err != nil || !created || first.Display.LastFour != "4242" || first.Provider != "stripe" {
		t.Fatalf("register: %+v %v %v", first, created, err)
	}
	again, created, err := fu.register.Run(ctx, a1, cardOf(ada.ID, "mfm_1"))
	if err != nil || created || again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("repeat: %+v %v %v", again, created, err)
	}
	wallet, _, err := fu.register.Run(ctx, a1, commerceapp.FundingMethodCommand{CustomerID: ada.ID, Reference: "mfm_2", Provider: "stripe", Brand: "paypal"})
	if err != nil || wallet.Display.LastFour != "" {
		t.Fatalf("a wallet without last four: %+v, %v", wallet, err)
	}
	if _, _, err := fu.register.Run(ctx, a1, commerceapp.FundingMethodCommand{CustomerID: ada.ID, Reference: "mfm_1", Provider: "adyen", Brand: "visa", LastFour: "4242"}); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("another provider: %v", err)
	}

	// Another merchant can't bind, see, list or change ada's.
	for _, other := range []commerceapp.MerchantPrincipal{a2, b} {
		if _, _, err := fu.register.Run(ctx, other, cardOf(ada.ID, "mfm_9")); !errors.Is(err, commerceapp.ErrCustomerUnknown) {
			t.Errorf("%s binding A1's customer: %v", other.MerchantID, err)
		}
		if _, err := fu.list.Run(ctx, other, ada.ID, commerceapp.ListFilter{}); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s listing A1's customer's bindings: %v", other.MerchantID, err)
		}
		if _, err := fu.status.Run(ctx, other, first.ID, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s disabling A1's binding: %v", other.MerchantID, err)
		}
	}
	if got, err := fu.store.GetFundingMethod(ctx, a1.Scope(), first.ID); err != nil || got.Status != commerce.Active {
		t.Errorf("A1's binding after the others' attempts: %+v, %v", got, err)
	}

	page, err := fu.list.Run(ctx, a1, ada.ID, commerceapp.ListFilter{Limit: 1})
	if err != nil || len(page.Methods) != 1 || page.Methods[0].ID != wallet.ID || page.NextCursor == "" {
		t.Fatalf("first page: %+v, %v", page, err)
	}
	page, _ = fu.list.Run(ctx, a1, ada.ID, commerceapp.ListFilter{Limit: 1, Cursor: page.NextCursor})
	if len(page.Methods) != 1 || page.Methods[0].ID != first.ID || page.NextCursor != "" {
		t.Errorf("second page: %+v", page)
	}

	if got, err := fu.status.Run(ctx, a1, first.ID, commerce.Disabled); err != nil || got.DisabledAt == nil {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	disabled, _ := fu.list.Run(ctx, a1, ada.ID, commerceapp.ListFilter{Status: commerce.Disabled})
	if len(disabled.Methods) != 1 || disabled.Methods[0].ID != first.ID {
		t.Errorf("disabled: %+v", disabled.Methods)
	}
}

// The database itself refuses a binding to another merchant's customer,
// and more of a card than its last four - whatever the code above it does.
func TestCommerceFundingMethods_TheSchemaRefusesWhatTheRulesForbid(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	fu := newFundingUseCases(t)
	accounts := NewAccountStore(testPool(t))
	org, _ := newTestOrg(t, accounts)
	mine, _ := u.newMerchantIn(t, org.ID)
	sibling, _ := u.newMerchantIn(t, org.ID)
	theirs, _, _ := u.registerCustomer.Run(ctx, sibling, "cust", "c@example.com")
	ours, _, _ := u.registerCustomer.Run(ctx, mine, "cust", "c@example.com")

	base := commerce.FundingMethod{
		ID: commerce.FundingMethodID(credentials.RandomIDs{}.NewID("fmb")), MerchantID: mine.MerchantID, OrgID: org.ID,
		CustomerID: theirs.ID, Reference: "mfm_schema", Provider: "stripe",
		Display: commerce.FundingDisplay{Brand: "visa", LastFour: "4242"}, Status: commerce.Active, CreatedAt: realClock{}.Now(),
	}
	if _, _, err := fu.store.RegisterFundingMethod(ctx, base); err == nil {
		t.Error("stored a binding of this merchant to a sibling storefront's customer")
	}
	base.CustomerID = ours.ID
	base.Display.LastFour = "4242424242424242"
	if _, _, err := fu.store.RegisterFundingMethod(ctx, base); err == nil {
		t.Error("stored a whole card number as last four")
	}
}

func TestCommerceFundingMethods_ConcurrentRegistrationsMakeOneBinding(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	fu := newFundingUseCases(t)
	m, _ := u.newMerchant(t, NewAccountStore(testPool(t)))
	c, _, _ := u.registerCustomer.Run(ctx, m, "cust", "c@example.com")

	var wg sync.WaitGroup
	ids := make([]commerce.FundingMethodID, 10)
	made := make([]bool, len(ids))
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, created, err := fu.register.Run(ctx, m, cardOf(c.ID, "mfm_race"))
			if err != nil {
				t.Errorf("registration %d: %v", i, err)
				return
			}
			ids[i], made[i] = f.ID, created
		}()
	}
	wg.Wait()
	n := 0
	for i := range ids {
		if ids[i] != ids[0] {
			t.Errorf("registration %d got %s, want %s", i, ids[i], ids[0])
		}
		if made[i] {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d registrations report creating the binding, want 1", n)
	}
}

func TestCommerceFundingMethods_ByThePSPsReference(t *testing.T) {
	ctx := context.Background()
	u, _, _ := newCommerceUseCases(t)
	fu := newFundingUseCases(t)
	m, _ := u.newMerchant(t, NewAccountStore(testPool(t)))
	c, _, _ := u.registerCustomer.Run(ctx, m, "cust", "c@example.com")
	byPSP := commerceapp.FundingMethodCommand{CustomerID: c.ID, ProviderReference: "pm_1", Provider: "stripe", Brand: "visa", LastFour: "4242"}

	first, created, err := fu.register.Run(ctx, m, byPSP)
	if err != nil || !created || first.Reference != "" || first.ProviderReference != "pm_1" {
		t.Fatalf("register: %+v %v %v", first, created, err)
	}
	if again, created, err := fu.register.Run(ctx, m, byPSP); err != nil || created || again.ID != first.ID {
		t.Errorf("repeat: %+v %v %v", again, created, err)
	}
	both := commerceapp.FundingMethodCommand{CustomerID: c.ID, Reference: "mfm_2", ProviderReference: "pm_2", Provider: "stripe", Brand: "visa", LastFour: "4242"}
	b, _, err := fu.register.Run(ctx, m, both)
	if err != nil {
		t.Fatal(err)
	}
	if got, created, err := fu.register.Run(ctx, m, commerceapp.FundingMethodCommand{CustomerID: c.ID, ProviderReference: "pm_2", Provider: "stripe", Brand: "visa", LastFour: "4242"}); err != nil || created || got.ID != b.ID || got.Reference != "mfm_2" {
		t.Errorf("by the PSP's reference alone: %+v %v %v", got, created, err)
	}
	mixed := commerceapp.FundingMethodCommand{CustomerID: c.ID, Reference: "mfm_2", ProviderReference: "pm_1", Provider: "stripe", Brand: "visa", LastFour: "4242"}
	if _, _, err := fu.register.Run(ctx, m, mixed); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("references of two bindings: %v", err)
	}
	adyen := byPSP
	adyen.Provider = "adyen"
	if got, created, err := fu.register.Run(ctx, m, adyen); err != nil || !created || got.ID == first.ID {
		t.Errorf("same ID at another PSP: %+v %v %v", got, created, err)
	}
	if _, err := testPool(t).Exec(ctx, `UPDATE commerce_funding_methods SET merchant_funding_method_id = NULL, provider_method_reference = NULL WHERE id = $1`, string(b.ID)); err == nil {
		t.Error("the schema allowed a binding with no reference")
	}

	// Concurrent registrations by the PSP's reference make one binding.
	var wg sync.WaitGroup
	ids := make([]commerce.FundingMethodID, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, _, err := fu.register.Run(ctx, m, commerceapp.FundingMethodCommand{CustomerID: c.ID, ProviderReference: "pm_race", Provider: "stripe", Brand: "visa"})
			if err != nil {
				t.Errorf("registration %d: %v", i, err)
			}
			ids[i] = f.ID
		}()
	}
	wg.Wait()
	for i := range ids {
		if ids[i] != ids[0] {
			t.Errorf("registration %d got %s, want %s", i, ids[i], ids[0])
		}
	}
}
