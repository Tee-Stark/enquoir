package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

var (
	_ commerceapp.IntegrationStore = (*MerchantIntegrationStore)(nil)
	_ commerceapp.CustomerStore    = (*CommerceCustomerStore)(nil)
	_ commerceapp.AgentStore       = (*CommerceAgentStore)(nil)
)

// disabledAt is the disabled_at a record moved to status should have.
func disabledAt(status commerce.Status, at time.Time) *time.Time {
	if status == commerce.Disabled {
		return &at
	}
	return nil
}

// MerchantIntegrationStore persists merchant integrations and their keys.
type MerchantIntegrationStore struct{ pool *pgxpool.Pool }

func NewMerchantIntegrationStore(pool *pgxpool.Pool) *MerchantIntegrationStore {
	return &MerchantIntegrationStore{pool: pool}
}

func insertIntegrationKey(ctx context.Context, tx pgx.Tx, k commerceapp.IntegrationKey) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO merchant_integration_keys (id, merchant_id, org_id, prefix, secret_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		k.ID, string(k.MerchantID), string(k.OrgID), k.Prefix, k.SecretHash, k.CreatedAt)
	return err
}

func (s *MerchantIntegrationStore) CreateIntegration(ctx context.Context, m commerce.Merchant, key commerceapp.IntegrationKey) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO merchant_integrations (id, org_id, name, status, created_at, disabled_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			string(m.ID), string(m.OrgID), m.Name, string(m.Status), m.CreatedAt, m.DisabledAt); err != nil {
			return fmt.Errorf("creating merchant integration: %w", err)
		}
		if err := insertIntegrationKey(ctx, tx, key); err != nil {
			return fmt.Errorf("creating integration key: %w", err)
		}
		return nil
	})
}

// integrationSelect joins each integration to its active key, if any.
const integrationSelect = `
	SELECT m.id, m.org_id, m.name, m.status, m.created_at, m.disabled_at,
	       k.id, k.prefix, k.secret_hash, k.created_at, k.last_used_at
	FROM merchant_integrations m
	LEFT JOIN merchant_integration_keys k ON k.merchant_id = m.id AND k.revoked_at IS NULL`

func scanIntegration(row pgx.Row) (commerceapp.Integration, error) {
	var i commerceapp.Integration
	var id, org, status string
	var keyID, prefix, hash *string
	var keyCreated, lastUsed *time.Time
	if err := row.Scan(&id, &org, &i.Name, &status, &i.CreatedAt, &i.DisabledAt, &keyID, &prefix, &hash, &keyCreated, &lastUsed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return commerceapp.Integration{}, app.ErrNotFound
		}
		return commerceapp.Integration{}, err
	}
	i.ID, i.OrgID, i.Status = commerce.MerchantID(id), domain.OrgID(org), commerce.Status(status)
	if keyID != nil {
		i.Key = &commerceapp.IntegrationKey{ID: *keyID, MerchantID: i.ID, OrgID: i.OrgID, Prefix: *prefix, SecretHash: *hash, CreatedAt: *keyCreated, LastUsedAt: lastUsed}
	}
	return i, nil
}

func (s *MerchantIntegrationStore) ListIntegrations(ctx context.Context, org domain.OrgID) ([]commerceapp.Integration, error) {
	rows, err := s.pool.Query(ctx, integrationSelect+` WHERE m.org_id = $1 ORDER BY m.created_at, m.id`, string(org))
	if err != nil {
		return nil, fmt.Errorf("listing merchant integrations: %w", err)
	}
	defer rows.Close()
	var out []commerceapp.Integration
	for rows.Next() {
		i, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *MerchantIntegrationStore) GetIntegration(ctx context.Context, org domain.OrgID, id commerce.MerchantID) (commerceapp.Integration, error) {
	return scanIntegration(s.pool.QueryRow(ctx, integrationSelect+` WHERE m.org_id = $1 AND m.id = $2`, string(org), string(id)))
}

// lockIntegration locks an organisation's integration for the rest of
// tx, so that key and status changes to one integration serialise.
func lockIntegration(ctx context.Context, tx pgx.Tx, org domain.OrgID, id commerce.MerchantID) (commerce.Status, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM merchant_integrations WHERE org_id = $1 AND id = $2 FOR UPDATE`,
		string(org), string(id)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", app.ErrNotFound
	}
	return commerce.Status(status), err
}

func (s *MerchantIntegrationStore) SetIntegrationStatus(ctx context.Context, org domain.OrgID, id commerce.MerchantID, status commerce.Status, at time.Time) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := lockIntegration(ctx, tx, org, id)
		if err != nil || current == status {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE merchant_integrations SET status = $2, disabled_at = $3 WHERE id = $1`,
			string(id), string(status), disabledAt(status, at))
		changed = err == nil
		return err
	})
	return changed, err
}

func (s *MerchantIntegrationStore) ReplaceKey(ctx context.Context, org domain.OrgID, id commerce.MerchantID, key commerceapp.IntegrationKey, at time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockIntegration(ctx, tx, org, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE merchant_integration_keys SET revoked_at = $2 WHERE merchant_id = $1 AND revoked_at IS NULL`, string(id), at); err != nil {
			return err
		}
		return insertIntegrationKey(ctx, tx, key)
	})
}

func (s *MerchantIntegrationStore) RevokeKey(ctx context.Context, org domain.OrgID, id commerce.MerchantID, at time.Time) (bool, error) {
	revoked := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockIntegration(ctx, tx, org, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE merchant_integration_keys SET revoked_at = $2 WHERE merchant_id = $1 AND revoked_at IS NULL`, string(id), at)
		revoked = err == nil && tag.RowsAffected() > 0
		return err
	})
	return revoked, err
}

func (s *MerchantIntegrationStore) ActiveKeyByHash(ctx context.Context, hash string) (commerceapp.IntegrationKey, commerce.Merchant, error) {
	i, err := scanIntegration(s.pool.QueryRow(ctx, integrationSelect+` WHERE k.secret_hash = $1 AND m.status = 'active'`, hash))
	if err != nil {
		return commerceapp.IntegrationKey{}, commerce.Merchant{}, err
	}
	return *i.Key, i.Merchant, nil
}

// TouchKey records use at most once a minute per key.
func (s *MerchantIntegrationStore) TouchKey(ctx context.Context, keyID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE merchant_integration_keys SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, keyID, at)
	return err
}

// directoryTable describes a register-or-resolve table of a merchant's
// records - customers or agents - by their one differing column.
type directoryTable struct {
	table, detail string
}

var (
	customersTable = directoryTable{table: "commerce_customers", detail: "email"}
	agentsTable    = directoryTable{table: "commerce_agents", detail: "name"}
)

func (d directoryTable) columns() string {
	return `id, merchant_id, org_id, external_id, ` + d.detail + `, status, created_at, disabled_at`
}

// directoryRow is one row of a directory table.
type directoryRow struct {
	id, merchant, org, external, detail, status string
	created                                     time.Time
	disabled                                    *time.Time
}

func scanDirectoryRow(row pgx.Row) (directoryRow, error) {
	var r directoryRow
	err := row.Scan(&r.id, &r.merchant, &r.org, &r.external, &r.detail, &r.status, &r.created, &r.disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return directoryRow{}, app.ErrNotFound
	}
	return r, err
}

// register inserts r unless its merchant already has its external ID,
// and returns the stored row either way. A concurrent insert of the same
// external ID waits for the first to commit, then reads it.
func (d directoryTable) register(ctx context.Context, pool *pgxpool.Pool, r directoryRow) (directoryRow, bool, error) {
	stored, err := scanDirectoryRow(pool.QueryRow(ctx, `
		INSERT INTO `+d.table+` (`+d.columns()+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (merchant_id, external_id) DO NOTHING
		RETURNING `+d.columns(),
		r.id, r.merchant, r.org, r.external, r.detail, r.status, r.created, r.disabled))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, app.ErrNotFound) {
		return directoryRow{}, false, err
	}
	stored, err = scanDirectoryRow(pool.QueryRow(ctx, `SELECT `+d.columns()+` FROM `+d.table+`
		WHERE org_id = $1 AND merchant_id = $2 AND external_id = $3`, r.org, r.merchant, r.external))
	return stored, false, err
}

func (d directoryTable) get(ctx context.Context, pool *pgxpool.Pool, s commerceapp.Scope, id string) (directoryRow, error) {
	return scanDirectoryRow(pool.QueryRow(ctx, `SELECT `+d.columns()+` FROM `+d.table+`
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3`, string(s.OrgID), string(s.MerchantID), id))
}

func (d directoryTable) setStatus(ctx context.Context, pool *pgxpool.Pool, s commerceapp.Scope, id string, status commerce.Status, at time.Time) (bool, error) {
	tag, err := pool.Exec(ctx, `UPDATE `+d.table+` SET status = $4, disabled_at = $5
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3 AND status <> $4`,
		string(s.OrgID), string(s.MerchantID), id, string(status), disabledAt(status, at))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	_, err = d.get(ctx, pool, s, id) // unchanged, or not this merchant's?
	return false, err
}

func (d directoryTable) list(ctx context.Context, pool *pgxpool.Pool, s commerceapp.Scope, f commerceapp.ListFilter) ([]directoryRow, string, error) {
	w := &where{}
	w.add("org_id = ?", string(s.OrgID))
	w.add("merchant_id = ?", string(s.MerchantID))
	if f.ExternalID != "" {
		w.add("external_id = ?", string(f.ExternalID))
	}
	if f.Status != "" {
		w.add("status = ?", string(f.Status))
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(created_at, id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	rows, err := pool.Query(ctx, `SELECT `+d.columns()+` FROM `+d.table+` WHERE `+w.sql()+
		` ORDER BY created_at DESC, id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing %s: %w", d.table, err)
	}
	defer rows.Close()
	var out []directoryRow
	for rows.Next() {
		r, err := scanDirectoryRow(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		last := out[f.Limit-1]
		next = encodeCursor(last.created, last.id)
	}
	return out, next, nil
}

// CommerceCustomerStore persists merchants' customers.
type CommerceCustomerStore struct{ pool *pgxpool.Pool }

func NewCommerceCustomerStore(pool *pgxpool.Pool) *CommerceCustomerStore {
	return &CommerceCustomerStore{pool: pool}
}

func (r directoryRow) customer() commerce.Customer {
	return commerce.Customer{
		ID: commerce.CustomerID(r.id), MerchantID: commerce.MerchantID(r.merchant), OrgID: domain.OrgID(r.org),
		ExternalID: commerce.ExternalID(r.external), Email: domain.Email(r.detail),
		Status: commerce.Status(r.status), CreatedAt: r.created, DisabledAt: r.disabled,
	}
}

func (s *CommerceCustomerStore) RegisterCustomer(ctx context.Context, c commerce.Customer) (commerce.Customer, bool, error) {
	r, created, err := customersTable.register(ctx, s.pool, directoryRow{
		id: string(c.ID), merchant: string(c.MerchantID), org: string(c.OrgID), external: string(c.ExternalID),
		detail: string(c.Email), status: string(c.Status), created: c.CreatedAt, disabled: c.DisabledAt,
	})
	if err != nil {
		return commerce.Customer{}, false, err
	}
	return r.customer(), created, nil
}

func (s *CommerceCustomerStore) GetCustomer(ctx context.Context, sc commerceapp.Scope, id commerce.CustomerID) (commerce.Customer, error) {
	r, err := customersTable.get(ctx, s.pool, sc, string(id))
	return r.customer(), err
}

func (s *CommerceCustomerStore) SetCustomerStatus(ctx context.Context, sc commerceapp.Scope, id commerce.CustomerID, status commerce.Status, at time.Time) (bool, error) {
	return customersTable.setStatus(ctx, s.pool, sc, string(id), status, at)
}

func (s *CommerceCustomerStore) ListCustomers(ctx context.Context, sc commerceapp.Scope, f commerceapp.ListFilter) (commerceapp.CustomerPage, error) {
	rows, next, err := customersTable.list(ctx, s.pool, sc, f)
	if err != nil {
		return commerceapp.CustomerPage{}, err
	}
	page := commerceapp.CustomerPage{NextCursor: next, Customers: make([]commerce.Customer, len(rows))}
	for i, r := range rows {
		page.Customers[i] = r.customer()
	}
	return page, nil
}

// CommerceAgentStore persists merchants' agents.
type CommerceAgentStore struct{ pool *pgxpool.Pool }

func NewCommerceAgentStore(pool *pgxpool.Pool) *CommerceAgentStore {
	return &CommerceAgentStore{pool: pool}
}

func (r directoryRow) agent() commerce.Agent {
	return commerce.Agent{
		ID: commerce.AgentID(r.id), MerchantID: commerce.MerchantID(r.merchant), OrgID: domain.OrgID(r.org),
		ExternalID: commerce.ExternalID(r.external), Name: r.detail,
		Status: commerce.Status(r.status), CreatedAt: r.created, DisabledAt: r.disabled,
	}
}

func (s *CommerceAgentStore) RegisterAgent(ctx context.Context, a commerce.Agent) (commerce.Agent, bool, error) {
	r, created, err := agentsTable.register(ctx, s.pool, directoryRow{
		id: string(a.ID), merchant: string(a.MerchantID), org: string(a.OrgID), external: string(a.ExternalID),
		detail: a.Name, status: string(a.Status), created: a.CreatedAt, disabled: a.DisabledAt,
	})
	if err != nil {
		return commerce.Agent{}, false, err
	}
	return r.agent(), created, nil
}

func (s *CommerceAgentStore) GetAgent(ctx context.Context, sc commerceapp.Scope, id commerce.AgentID) (commerce.Agent, error) {
	r, err := agentsTable.get(ctx, s.pool, sc, string(id))
	return r.agent(), err
}

func (s *CommerceAgentStore) SetAgentStatus(ctx context.Context, sc commerceapp.Scope, id commerce.AgentID, status commerce.Status, at time.Time) (bool, error) {
	return agentsTable.setStatus(ctx, s.pool, sc, string(id), status, at)
}

func (s *CommerceAgentStore) ListAgents(ctx context.Context, sc commerceapp.Scope, f commerceapp.ListFilter) (commerceapp.AgentPage, error) {
	rows, next, err := agentsTable.list(ctx, s.pool, sc, f)
	if err != nil {
		return commerceapp.AgentPage{}, err
	}
	page := commerceapp.AgentPage{NextCursor: next, Agents: make([]commerce.Agent, len(rows))}
	for i, r := range rows {
		page.Agents[i] = r.agent()
	}
	return page, nil
}

var _ commerceapp.FundingMethodStore = (*CommerceFundingMethodStore)(nil)

// CommerceFundingMethodStore persists funding-method bindings.
type CommerceFundingMethodStore struct{ pool *pgxpool.Pool }

func NewCommerceFundingMethodStore(pool *pgxpool.Pool) *CommerceFundingMethodStore {
	return &CommerceFundingMethodStore{pool: pool}
}

const fundingColumns = `id, merchant_id, org_id, customer_id, merchant_funding_method_id, provider,
	display_brand, COALESCE(display_last_four, ''), status, created_at, disabled_at`

func scanFundingMethod(row pgx.Row) (commerce.FundingMethod, error) {
	var f commerce.FundingMethod
	var id, merchant, org, customer, ref, provider, status string
	err := row.Scan(&id, &merchant, &org, &customer, &ref, &provider, &f.Display.Brand, &f.Display.LastFour, &status, &f.CreatedAt, &f.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.FundingMethod{}, app.ErrNotFound
	}
	if err != nil {
		return commerce.FundingMethod{}, err
	}
	f.ID, f.MerchantID, f.OrgID, f.CustomerID = commerce.FundingMethodID(id), commerce.MerchantID(merchant), domain.OrgID(org), commerce.CustomerID(customer)
	f.Reference, f.Provider, f.Status = commerce.FundingReference(ref), commerce.Provider(provider), commerce.Status(status)
	return f, nil
}

// RegisterFundingMethod inserts f unless its merchant already has its
// reference, then returns the stored binding; concurrent registrations
// of one reference make one binding.
func (s *CommerceFundingMethodStore) RegisterFundingMethod(ctx context.Context, f commerce.FundingMethod) (commerce.FundingMethod, bool, error) {
	stored, err := scanFundingMethod(s.pool.QueryRow(ctx, `
		INSERT INTO commerce_funding_methods (id, merchant_id, org_id, customer_id, merchant_funding_method_id, provider,
			display_brand, display_last_four, status, created_at, disabled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (merchant_id, merchant_funding_method_id) DO NOTHING
		RETURNING `+fundingColumns,
		string(f.ID), string(f.MerchantID), string(f.OrgID), string(f.CustomerID), string(f.Reference), string(f.Provider),
		f.Display.Brand, nullable(f.Display.LastFour), string(f.Status), f.CreatedAt, f.DisabledAt))
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, app.ErrNotFound) {
		return commerce.FundingMethod{}, false, err
	}
	stored, err = scanFundingMethod(s.pool.QueryRow(ctx, `SELECT `+fundingColumns+` FROM commerce_funding_methods
		WHERE org_id = $1 AND merchant_id = $2 AND merchant_funding_method_id = $3`,
		string(f.OrgID), string(f.MerchantID), string(f.Reference)))
	return stored, false, err
}

func (s *CommerceFundingMethodStore) GetFundingMethod(ctx context.Context, sc commerceapp.Scope, id commerce.FundingMethodID) (commerce.FundingMethod, error) {
	return scanFundingMethod(s.pool.QueryRow(ctx, `SELECT `+fundingColumns+` FROM commerce_funding_methods
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3`, string(sc.OrgID), string(sc.MerchantID), string(id)))
}

func (s *CommerceFundingMethodStore) SetFundingMethodStatus(ctx context.Context, sc commerceapp.Scope, id commerce.FundingMethodID, status commerce.Status, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE commerce_funding_methods SET status = $4, disabled_at = $5
		WHERE org_id = $1 AND merchant_id = $2 AND id = $3 AND status <> $4`,
		string(sc.OrgID), string(sc.MerchantID), string(id), string(status), disabledAt(status, at))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	_, err = s.GetFundingMethod(ctx, sc, id)
	return false, err
}

func (s *CommerceFundingMethodStore) ListFundingMethods(ctx context.Context, sc commerceapp.Scope, customer commerce.CustomerID, f commerceapp.ListFilter) (commerceapp.FundingMethodPage, error) {
	w := &where{}
	w.add("org_id = ?", string(sc.OrgID))
	w.add("merchant_id = ?", string(sc.MerchantID))
	w.add("customer_id = ?", string(customer))
	if f.Status != "" {
		w.add("status = ?", string(f.Status))
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return commerceapp.FundingMethodPage{}, &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(created_at, id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	rows, err := s.pool.Query(ctx, `SELECT `+fundingColumns+` FROM commerce_funding_methods WHERE `+w.sql()+
		` ORDER BY created_at DESC, id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return commerceapp.FundingMethodPage{}, fmt.Errorf("listing funding methods: %w", err)
	}
	defer rows.Close()
	var page commerceapp.FundingMethodPage
	for rows.Next() {
		m, err := scanFundingMethod(rows)
		if err != nil {
			return commerceapp.FundingMethodPage{}, err
		}
		page.Methods = append(page.Methods, m)
	}
	if err := rows.Err(); err != nil {
		return commerceapp.FundingMethodPage{}, err
	}
	if len(page.Methods) > f.Limit {
		page.Methods = page.Methods[:f.Limit]
		last := page.Methods[f.Limit-1]
		page.NextCursor = encodeCursor(last.CreatedAt, string(last.ID))
	}
	return page, nil
}
