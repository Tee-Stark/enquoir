package commerceapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
)

const (
	// integrationKeyPrefix marks a merchant integration key. Treasury
	// agent keys are enq_sk_; neither is accepted in the other's place.
	integrationKeyPrefix = "enq_mk"
	keyDisplayLength     = 12
)

// IntegrationKey is a merchant integration's credential. Only its hash
// is stored; Prefix is the start of the secret, for people to
// recognise it by.
type IntegrationKey struct {
	ID         string
	MerchantID commerce.MerchantID
	OrgID      domain.OrgID
	Prefix     string
	SecretHash string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// Integration is a merchant integration with its active key (nil once
// revoked, until a new one is issued).
type Integration struct {
	commerce.Merchant
	Key *IntegrationKey
}

// IntegrationStore persists merchant integrations and their keys. Every
// method but ActiveKeyByHash and TouchKey is scoped by organisation:
// another organisation's integration is app.ErrNotFound.
type IntegrationStore interface {
	// CreateIntegration stores an integration and its first key together.
	CreateIntegration(ctx context.Context, m commerce.Merchant, key IntegrationKey) error
	ListIntegrations(ctx context.Context, org domain.OrgID) ([]Integration, error)
	GetIntegration(ctx context.Context, org domain.OrgID, id commerce.MerchantID) (Integration, error)
	// SetIntegrationStatus moves an integration to status, stamping
	// DisabledAt when disabling and clearing it when enabling. changed
	// is false if it already had that status.
	SetIntegrationStatus(ctx context.Context, org domain.OrgID, id commerce.MerchantID, status commerce.Status, at time.Time) (changed bool, err error)
	// ReplaceKey revokes the active key, if any, and stores key.
	ReplaceKey(ctx context.Context, org domain.OrgID, id commerce.MerchantID, key IntegrationKey, at time.Time) error
	// RevokeKey revokes the active key. revoked is false if there was none.
	RevokeKey(ctx context.Context, org domain.OrgID, id commerce.MerchantID, at time.Time) (revoked bool, err error)
	// ActiveKeyByHash finds an unrevoked key of an active integration.
	ActiveKeyByHash(ctx context.Context, hash string) (IntegrationKey, commerce.Merchant, error)
	// TouchKey records a key's use.
	TouchKey(ctx context.Context, keyID string, at time.Time) error
}

// IssuedIntegration is an integration with a newly issued key. Secret
// is shown to the caller once and never stored.
type IssuedIntegration struct {
	Integration Integration
	Secret      string
}

func newIntegrationKey(ids app.IDGenerator, secrets app.SecretGenerator, org domain.OrgID, id commerce.MerchantID, now time.Time) (IntegrationKey, string) {
	secret := secrets.NewSecret(integrationKeyPrefix)
	return IntegrationKey{
		ID:         ids.NewID("mkey"),
		MerchantID: id,
		OrgID:      org,
		Prefix:     secret[:keyDisplayLength],
		SecretHash: app.HashSecret(secret),
		CreatedAt:  now,
	}, secret
}

func invalid(field string, err error) error {
	return &app.ValidationError{Field: field, Message: err.Error()}
}

const auditTargetIntegration = "merchant_integration"

// merchantOwner is the cache owner of everything cached for one
// merchant integration: invalidating it ends every cached key at once.
func merchantOwner(id commerce.MerchantID) string { return "merchant:" + string(id) }
func merchantKeyKey(hash string) string           { return "mik:" + hash }

// CreateIntegration connects a new merchant and issues its key.
type CreateIntegration struct {
	Integrations IntegrationStore
	IDs          app.IDGenerator
	Secrets      app.SecretGenerator
	Clock        app.Clock
	Audit        *app.Audit
}

func (c *CreateIntegration) Run(ctx context.Context, org domain.OrgID, name string) (IssuedIntegration, error) {
	name, err := domain.NewName(name)
	if err != nil {
		return IssuedIntegration{}, invalid("name", err)
	}
	if commerce.HoldsCardNumber(name) {
		return IssuedIntegration{}, invalid("name", fmt.Errorf("name %w", commerce.ErrLooksLikeCardNumber))
	}
	now := c.Clock.Now()
	m := commerce.Merchant{ID: commerce.MerchantID(c.IDs.NewID("mi")), OrgID: org, Name: name, Status: commerce.Active, CreatedAt: now}
	key, secret := newIntegrationKey(c.IDs, c.Secrets, org, m.ID, now)
	if err := c.Integrations.CreateIntegration(ctx, m, key); err != nil {
		return IssuedIntegration{}, fmt.Errorf("creating merchant integration: %w", err)
	}
	c.Audit.Record(ctx, org, app.AuditIntegrationCreated, auditTargetIntegration, string(m.ID), map[string]string{"name": name, "key_prefix": key.Prefix})
	return IssuedIntegration{Integration: Integration{Merchant: m, Key: &key}, Secret: secret}, nil
}

// ListIntegrations lists an organisation's integrations, disabled ones
// included.
type ListIntegrations struct{ Integrations IntegrationStore }

func (l *ListIntegrations) Run(ctx context.Context, org domain.OrgID) ([]Integration, error) {
	return l.Integrations.ListIntegrations(ctx, org)
}

// GetIntegration fetches one of an organisation's integrations.
type GetIntegration struct{ Integrations IntegrationStore }

func (g *GetIntegration) Run(ctx context.Context, org domain.OrgID, id commerce.MerchantID) (Integration, error) {
	return g.Integrations.GetIntegration(ctx, org, id)
}

// SetIntegrationStatus disables or enables an integration. Disabling
// stops its key at once; enabling lets the active key, if any, work
// again.
type SetIntegrationStatus struct {
	Integrations IntegrationStore
	Clock        app.Clock
	Cache        *app.Cache
	Audit        *app.Audit
}

func (s *SetIntegrationStatus) Run(ctx context.Context, org domain.OrgID, id commerce.MerchantID, status commerce.Status) (Integration, error) {
	status, err := commerce.ParseStatus(string(status))
	if err != nil {
		return Integration{}, invalid("status", err)
	}
	changed, err := s.Integrations.SetIntegrationStatus(ctx, org, id, status, s.Clock.Now())
	if err != nil {
		return Integration{}, err
	}
	if changed {
		s.Cache.Invalidate(ctx, merchantOwner(id))
		action := app.AuditIntegrationEnabled
		if status == commerce.Disabled {
			action = app.AuditIntegrationDisabled
		}
		s.Audit.Record(ctx, org, action, auditTargetIntegration, string(id), nil)
	}
	return s.Integrations.GetIntegration(ctx, org, id)
}

// RotateIntegrationKey issues a new key, revoking the active one, which
// stops working at once. It also issues a key to an integration whose
// key was revoked.
type RotateIntegrationKey struct {
	Integrations IntegrationStore
	IDs          app.IDGenerator
	Secrets      app.SecretGenerator
	Clock        app.Clock
	Cache        *app.Cache
	Audit        *app.Audit
}

func (r *RotateIntegrationKey) Run(ctx context.Context, org domain.OrgID, id commerce.MerchantID) (IssuedIntegration, error) {
	now := r.Clock.Now()
	key, secret := newIntegrationKey(r.IDs, r.Secrets, org, id, now)
	if err := r.Integrations.ReplaceKey(ctx, org, id, key, now); err != nil {
		return IssuedIntegration{}, err
	}
	r.Cache.Invalidate(ctx, merchantOwner(id))
	r.Audit.Record(ctx, org, app.AuditIntegrationKeyRotated, auditTargetIntegration, string(id), map[string]string{"key_prefix": key.Prefix})
	got, err := r.Integrations.GetIntegration(ctx, org, id)
	if err != nil {
		return IssuedIntegration{}, err
	}
	return IssuedIntegration{Integration: got, Secret: secret}, nil
}

// RevokeIntegrationKey revokes an integration's key without issuing a
// new one: the merchant is cut off until a key is rotated in.
type RevokeIntegrationKey struct {
	Integrations IntegrationStore
	Clock        app.Clock
	Cache        *app.Cache
	Audit        *app.Audit
}

func (r *RevokeIntegrationKey) Run(ctx context.Context, org domain.OrgID, id commerce.MerchantID) error {
	revoked, err := r.Integrations.RevokeKey(ctx, org, id, r.Clock.Now())
	if err != nil {
		return err
	}
	if revoked {
		r.Cache.Invalidate(ctx, merchantOwner(id))
		r.Audit.Record(ctx, org, app.AuditIntegrationKeyRevoked, auditTargetIntegration, string(id), nil)
	}
	return nil
}

// MerchantPrincipal is an authenticated merchant integration: who is
// calling the commerce API. Every commerce read and write is scoped by
// it, never by anything in a request body.
type MerchantPrincipal struct {
	MerchantID   commerce.MerchantID
	MerchantName string
	OrgID        domain.OrgID
	KeyID        string
}

// AuthenticateMerchant turns an integration key into its merchant.
type AuthenticateMerchant struct {
	Integrations IntegrationStore
	Clock        app.Clock
	Cache        *app.Cache
}

// Run is on every commerce request. A key without the integration-key
// prefix (a treasury agent key, say) is refused without a lookup. A
// cached key skips the database and the last-used write.
func (a *AuthenticateMerchant) Run(ctx context.Context, secret string) (MerchantPrincipal, error) {
	if !strings.HasPrefix(secret, integrationKeyPrefix+"_") {
		return MerchantPrincipal{}, app.ErrUnauthenticated
	}
	hash := app.HashSecret(secret)
	var p MerchantPrincipal
	if a.Cache.Lookup(ctx, merchantKeyKey(hash), &p) && p.MerchantID != "" {
		return p, nil
	}
	key, m, err := a.Integrations.ActiveKeyByHash(ctx, hash)
	if errors.Is(err, app.ErrNotFound) {
		return MerchantPrincipal{}, app.ErrUnauthenticated
	}
	if err != nil {
		return MerchantPrincipal{}, err
	}
	if err := a.Integrations.TouchKey(ctx, key.ID, a.Clock.Now()); err != nil {
		return MerchantPrincipal{}, err
	}
	p = MerchantPrincipal{MerchantID: m.ID, MerchantName: m.Name, OrgID: m.OrgID, KeyID: key.ID}
	a.Cache.Remember(ctx, merchantKeyKey(hash), merchantOwner(m.ID), p)
	return p, nil
}
