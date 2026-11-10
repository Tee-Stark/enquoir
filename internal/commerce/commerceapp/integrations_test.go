package commerceapp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/commerce/commerceapp/commercetest"
	"enquoir/internal/domain"
)

const orgA, orgB = domain.OrgID("org_a"), domain.OrgID("org_b")

type integrationsFixture struct {
	store  *commercetest.Integrations
	audit  *apptest.AuditLog
	cache  *apptest.PrincipalStore
	create *commerceapp.CreateIntegration
	list   *commerceapp.ListIntegrations
	get    *commerceapp.GetIntegration
	status *commerceapp.SetIntegrationStatus
	rotate *commerceapp.RotateIntegrationKey
	revoke *commerceapp.RevokeIntegrationKey
	auth   *commerceapp.AuthenticateMerchant
}

// newIntegrationsFixture wires every use case to one store, one audit
// log and, if cached, one shared principal cache.
func newIntegrationsFixture(cached bool) *integrationsFixture {
	store, clock, log := commercetest.NewIntegrations(), apptest.NewClock(), apptest.NewAuditLog()
	ids, secrets := &apptest.IDs{}, &apptest.Secrets{}
	audit := &app.Audit{Log: log, IDs: ids, Clock: clock}
	var cache *app.Cache
	var principals *apptest.PrincipalStore
	if cached {
		principals = apptest.NewPrincipalStore()
		cache = &app.Cache{Store: principals}
	}
	return &integrationsFixture{
		store: store, audit: log, cache: principals,
		create: &commerceapp.CreateIntegration{Integrations: store, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		list:   &commerceapp.ListIntegrations{Integrations: store},
		get:    &commerceapp.GetIntegration{Integrations: store},
		status: &commerceapp.SetIntegrationStatus{Integrations: store, Clock: clock, Cache: cache, Audit: audit},
		rotate: &commerceapp.RotateIntegrationKey{Integrations: store, IDs: ids, Secrets: secrets, Clock: clock, Cache: cache, Audit: audit},
		revoke: &commerceapp.RevokeIntegrationKey{Integrations: store, Clock: clock, Cache: cache, Audit: audit},
		auth:   &commerceapp.AuthenticateMerchant{Integrations: store, Clock: clock, Cache: cache},
	}
}

func (f *integrationsFixture) mustCreate(t *testing.T, org domain.OrgID, name string) commerceapp.IssuedIntegration {
	t.Helper()
	issued, err := f.create.Run(context.Background(), org, name)
	if err != nil {
		t.Fatalf("creating integration: %v", err)
	}
	return issued
}

func (f *integrationsFixture) actions() []string {
	var out []string
	for _, e := range f.audit.All() {
		out = append(out, e.Action)
	}
	return out
}

func TestCreateIntegration_IssuesAMerchantKeyShownOnce(t *testing.T) {
	f := newIntegrationsFixture(false)
	got := f.mustCreate(t, orgA, "  Acme Electronics  ")

	m := got.Integration
	if m.Name != "Acme Electronics" || m.OrgID != orgA || m.Status != commerce.Active || m.ID == "" {
		t.Errorf("integration = %+v", m.Merchant)
	}
	if !strings.HasPrefix(got.Secret, "enq_mk_") {
		t.Errorf("secret = %q, want an enq_mk_ key", got.Secret)
	}
	if m.Key == nil || len(m.Key.Prefix) != 12 || !strings.HasPrefix(got.Secret, m.Key.Prefix) {
		t.Errorf("key = %+v, want a 12-character display prefix of the secret", m.Key)
	}
	if f.store.StoresPlaintext(got.Secret) {
		t.Error("the integration key was stored in plaintext; only its hash may be")
	}
	events := f.audit.All()
	if len(events) != 1 || events[0].Action != app.AuditIntegrationCreated || events[0].OrgID != orgA ||
		events[0].TargetID != string(m.ID) || events[0].Details["key_prefix"] != m.Key.Prefix {
		t.Errorf("audit = %+v", events)
	}
}

func TestCreateIntegration_ValidatesTheName(t *testing.T) {
	f := newIntegrationsFixture(false)
	_, err := f.create.Run(context.Background(), orgA, "   ")
	var verr *app.ValidationError
	if !errors.As(err, &verr) || verr.Field != "name" {
		t.Fatalf("blank name: %v, want a validation error on name", err)
	}
}

func TestIntegrations_AreScopedToTheirOrganisation(t *testing.T) {
	f := newIntegrationsFixture(false)
	ctx := context.Background()
	a := f.mustCreate(t, orgA, "A shop").Integration
	f.mustCreate(t, orgA, "A second shop")
	f.mustCreate(t, orgB, "B shop")

	list, err := f.list.Run(ctx, orgA)
	if err != nil || len(list) != 2 {
		t.Fatalf("org A lists %d integrations (%v), want its own 2", len(list), err)
	}
	for _, m := range list {
		if m.OrgID != orgA {
			t.Errorf("org A's list holds %+v", m.Merchant)
		}
	}
	if _, err := f.get.Run(ctx, orgB, a.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("org B reading A's integration: %v, want not found", err)
	}
	if _, err := f.status.Run(ctx, orgB, a.ID, commerce.Disabled); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("org B disabling A's integration: %v, want not found", err)
	}
	if _, err := f.rotate.Run(ctx, orgB, a.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("org B rotating A's key: %v, want not found", err)
	}
	if err := f.revoke.Run(ctx, orgB, a.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("org B revoking A's key: %v, want not found", err)
	}
	if got, _ := f.get.Run(ctx, orgA, a.ID); got.Status != commerce.Active || got.Key == nil {
		t.Errorf("A's integration after org B's attempts: %+v", got)
	}
}

func TestAuthenticateMerchant_TurnsAKeyIntoItsMerchant(t *testing.T) {
	f := newIntegrationsFixture(false)
	issued := f.mustCreate(t, orgA, "Acme")
	p, err := f.auth.Run(context.Background(), issued.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.MerchantID != issued.Integration.ID || p.OrgID != orgA || p.MerchantName != "Acme" || p.KeyID != issued.Integration.Key.ID {
		t.Errorf("principal = %+v", p)
	}
}

// A treasury agent key is refused on commerce, without even a lookup:
// the two kinds of credential are never interchangeable.
func TestAuthenticateMerchant_RefusesUnknownAndTreasuryKeys(t *testing.T) {
	f := newIntegrationsFixture(false)
	f.mustCreate(t, orgA, "Acme")
	for _, secret := range []string{"", "enq_mk_made_up", "enq_sk_secret0001", "Bearer enq_mk_x"} {
		if _, err := f.auth.Run(context.Background(), secret); !errors.Is(err, app.ErrUnauthenticated) {
			t.Errorf("%q: %v, want unauthenticated", secret, err)
		}
	}
	if n := f.store.LookupsOf("enq_sk_secret0001"); n != 0 {
		t.Errorf("a treasury key was looked up %d times; want it refused by its prefix", n)
	}
}

func TestSetIntegrationStatus_DisablingStopsTheKeyAndEnablingRestoresIt(t *testing.T) {
	f := newIntegrationsFixture(false)
	ctx := context.Background()
	issued := f.mustCreate(t, orgA, "Acme")
	id := issued.Integration.ID

	got, err := f.status.Run(ctx, orgA, id, commerce.Disabled)
	if err != nil || got.Status != commerce.Disabled || got.DisabledAt == nil {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	if _, err := f.auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("a disabled integration's key: %v, want unauthenticated", err)
	}
	// Disabling again changes nothing and records nothing new.
	if _, err := f.status.Run(ctx, orgA, id, commerce.Disabled); err != nil {
		t.Fatal(err)
	}
	got, err = f.status.Run(ctx, orgA, id, commerce.Active)
	if err != nil || got.Status != commerce.Active || got.DisabledAt != nil {
		t.Fatalf("enable: %+v, %v", got, err)
	}
	if _, err := f.auth.Run(ctx, issued.Secret); err != nil {
		t.Errorf("the key after enabling: %v", err)
	}
	want := []string{app.AuditIntegrationCreated, app.AuditIntegrationDisabled, app.AuditIntegrationEnabled}
	if got := f.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestSetIntegrationStatus_RefusesAnUnknownStatus(t *testing.T) {
	f := newIntegrationsFixture(false)
	id := f.mustCreate(t, orgA, "Acme").Integration.ID
	var verr *app.ValidationError
	if _, err := f.status.Run(context.Background(), orgA, id, "paused"); !errors.As(err, &verr) {
		t.Errorf("status paused: %v, want a validation error", err)
	}
}

func TestRotateIntegrationKey_TheOldKeyStopsAndTheNewOneWorks(t *testing.T) {
	f := newIntegrationsFixture(false)
	ctx := context.Background()
	first := f.mustCreate(t, orgA, "Acme")

	rotated, err := f.rotate.Run(ctx, orgA, first.Integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Secret == first.Secret || !strings.HasPrefix(rotated.Secret, "enq_mk_") {
		t.Fatalf("rotated secret = %q", rotated.Secret)
	}
	if _, err := f.auth.Run(ctx, first.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("the old key after rotation: %v, want unauthenticated", err)
	}
	if p, err := f.auth.Run(ctx, rotated.Secret); err != nil || p.MerchantID != first.Integration.ID {
		t.Errorf("the new key: %+v, %v", p, err)
	}
	if rotated.Integration.Key == nil || rotated.Integration.Key.ID == first.Integration.Key.ID {
		t.Errorf("rotated key = %+v", rotated.Integration.Key)
	}
}

func TestRevokeIntegrationKey_LeavesNoWorkingKeyUntilANewOneIsIssued(t *testing.T) {
	f := newIntegrationsFixture(false)
	ctx := context.Background()
	issued := f.mustCreate(t, orgA, "Acme")
	id := issued.Integration.ID

	if err := f.revoke.Run(ctx, orgA, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("a revoked key: %v, want unauthenticated", err)
	}
	if got, _ := f.get.Run(ctx, orgA, id); got.Key != nil || got.Status != commerce.Active {
		t.Errorf("after revoking: %+v, want an active integration with no key", got)
	}
	// Revoking again is not an error, and records nothing new.
	if err := f.revoke.Run(ctx, orgA, id); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.rotate.Run(ctx, orgA, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Run(ctx, fresh.Secret); err != nil {
		t.Errorf("a key issued after revoking: %v", err)
	}
	want := []string{app.AuditIntegrationCreated, app.AuditIntegrationKeyRevoked, app.AuditIntegrationKeyRotated}
	if got := f.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

// Every change that ends a key's access ends it at once, cache
// included - and a request racing the change can't re-cache the old
// access: the change is committed first, the cache cleared after.
func TestIntegrationKeys_EveryAccessChangeEndsCachedAccessAtOnce(t *testing.T) {
	cases := map[string]func(f *integrationsFixture, id commerce.MerchantID) error{
		"disable": func(f *integrationsFixture, id commerce.MerchantID) error {
			_, err := f.status.Run(context.Background(), orgA, id, commerce.Disabled)
			return err
		},
		"rotate": func(f *integrationsFixture, id commerce.MerchantID) error {
			_, err := f.rotate.Run(context.Background(), orgA, id)
			return err
		},
		"revoke": func(f *integrationsFixture, id commerce.MerchantID) error {
			return f.revoke.Run(context.Background(), orgA, id)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newIntegrationsFixture(true)
			ctx := context.Background()
			issued := f.mustCreate(t, orgA, "Acme")
			if _, err := f.auth.Run(ctx, issued.Secret); err != nil {
				t.Fatal(err)
			}
			before := f.cache.Hits
			if _, err := f.auth.Run(ctx, issued.Secret); err != nil || f.cache.Hits != before+1 {
				t.Fatalf("second authentication: %v, hits %d; want a cache hit", err, f.cache.Hits-before)
			}

			// The racing request authenticates at the worst moment.
			f.store.BeforeChange = func() { f.auth.Run(ctx, issued.Secret) }
			if err := change(f, issued.Integration.ID); err != nil {
				t.Fatal(err)
			}
			f.store.BeforeChange = nil
			if _, err := f.auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
				t.Errorf("after %s the key still works from the cache: %v", name, err)
			}
		})
	}
}
