-- Merchant-delegated commerce, phase 1: merchant integrations, their
-- keys, and the customers and agents each merchant registers. A bounded
-- context of its own: nothing here references treasury's intents,
-- agents or beneficiaries.

CREATE TABLE merchant_integrations (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organisations(id),
    name        TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at  TIMESTAMPTZ NOT NULL,
    disabled_at TIMESTAMPTZ,
    CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
    -- Lets every commerce row name its merchant and organisation
    -- together, so the two can never disagree.
    UNIQUE (id, org_id)
);
CREATE INDEX merchant_integrations_org ON merchant_integrations (org_id, created_at);

CREATE TABLE merchant_integration_keys (
    id           TEXT PRIMARY KEY,
    merchant_id  TEXT NOT NULL,
    org_id       TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    secret_hash  TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    FOREIGN KEY (merchant_id, org_id) REFERENCES merchant_integrations (id, org_id)
);
-- At most one active key per integration.
CREATE UNIQUE INDEX merchant_integration_keys_one_active
    ON merchant_integration_keys (merchant_id) WHERE revoked_at IS NULL;

CREATE TABLE commerce_customers (
    id          TEXT PRIMARY KEY,
    merchant_id TEXT NOT NULL,
    org_id      TEXT NOT NULL,
    external_id TEXT NOT NULL,
    email       TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at  TIMESTAMPTZ NOT NULL,
    disabled_at TIMESTAMPTZ,
    CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
    FOREIGN KEY (merchant_id, org_id) REFERENCES merchant_integrations (id, org_id),
    -- The merchant's namespace: one external ID, one customer.
    UNIQUE (merchant_id, external_id)
);
CREATE INDEX commerce_customers_list ON commerce_customers (merchant_id, created_at DESC, id DESC);

CREATE TABLE commerce_agents (
    id          TEXT PRIMARY KEY,
    merchant_id TEXT NOT NULL,
    org_id      TEXT NOT NULL,
    external_id TEXT NOT NULL,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at  TIMESTAMPTZ NOT NULL,
    disabled_at TIMESTAMPTZ,
    CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
    FOREIGN KEY (merchant_id, org_id) REFERENCES merchant_integrations (id, org_id),
    UNIQUE (merchant_id, external_id)
);
CREATE INDEX commerce_agents_list ON commerce_agents (merchant_id, created_at DESC, id DESC);

-- When a merchant acts with its integration key, the audit log names it.
ALTER TABLE audit_events
    ADD COLUMN actor_merchant_id   TEXT REFERENCES merchant_integrations(id),
    ADD COLUMN actor_merchant_name TEXT;
