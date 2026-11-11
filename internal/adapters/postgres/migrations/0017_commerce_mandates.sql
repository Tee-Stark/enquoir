-- Merchant-delegated commerce, phase 3: mandate requests, the customer's
-- consent records, and mandates. Terms are stored as the exact canonical
-- snapshot text (not JSONB, which would reorder it), and the database
-- checks each stored hash really is the snapshot's.

-- Let requests name an agent, and a card of a given customer, together
-- with their merchant and organisation.
ALTER TABLE commerce_agents
    ADD CONSTRAINT commerce_agents_id_merchant_org UNIQUE (id, merchant_id, org_id);
ALTER TABLE commerce_funding_methods
    ADD CONSTRAINT commerce_funding_methods_id_customer_merchant_org UNIQUE (id, customer_id, merchant_id, org_id);

CREATE TABLE commerce_mandate_requests (
    id                TEXT PRIMARY KEY,
    merchant_id       TEXT NOT NULL,
    org_id            TEXT NOT NULL,
    external_id       TEXT NOT NULL,
    customer_id       TEXT NOT NULL,
    agent_id          TEXT NOT NULL,
    funding_method_id TEXT NOT NULL,
    terms             TEXT NOT NULL,
    terms_hash        TEXT NOT NULL CHECK (terms_hash = 'sha256:' || encode(sha256(convert_to(terms, 'UTF8')), 'hex')),
    status            TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
    created_at        TIMESTAMPTZ NOT NULL,
    expires_at        TIMESTAMPTZ NOT NULL,
    decided_at        TIMESTAMPTZ,
    CHECK ((status = 'pending') = (decided_at IS NULL)),
    FOREIGN KEY (merchant_id, org_id) REFERENCES merchant_integrations (id, org_id),
    FOREIGN KEY (customer_id, merchant_id, org_id) REFERENCES commerce_customers (id, merchant_id, org_id),
    FOREIGN KEY (agent_id, merchant_id, org_id) REFERENCES commerce_agents (id, merchant_id, org_id),
    -- The card must be this customer's.
    FOREIGN KEY (funding_method_id, customer_id, merchant_id, org_id)
        REFERENCES commerce_funding_methods (id, customer_id, merchant_id, org_id),
    UNIQUE (merchant_id, external_id)
);

-- Evidence of a customer's decision: one per request, never changed.
CREATE TABLE commerce_consent_records (
    id          TEXT PRIMARY KEY,
    request_id  TEXT NOT NULL UNIQUE REFERENCES commerce_mandate_requests(id),
    org_id      TEXT NOT NULL,
    merchant_id TEXT NOT NULL,
    customer_id TEXT NOT NULL,
    decision    TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
    snapshot    TEXT NOT NULL,
    terms_hash  TEXT NOT NULL CHECK (terms_hash = 'sha256:' || encode(sha256(convert_to(snapshot, 'UTF8')), 'hex')),
    method      TEXT NOT NULL,
    ip          TEXT,
    user_agent  TEXT,
    at          TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (customer_id, merchant_id, org_id) REFERENCES commerce_customers (id, merchant_id, org_id)
);

CREATE FUNCTION commerce_consent_records_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'consent records are immutable';
END $$;
CREATE TRIGGER commerce_consent_records_immutable
    BEFORE UPDATE OR DELETE ON commerce_consent_records
    FOR EACH ROW EXECUTE FUNCTION commerce_consent_records_immutable();

CREATE TABLE commerce_mandates (
    id                TEXT PRIMARY KEY,
    request_id        TEXT NOT NULL UNIQUE REFERENCES commerce_mandate_requests(id),
    consent_record_id TEXT NOT NULL UNIQUE REFERENCES commerce_consent_records(id),
    merchant_id       TEXT NOT NULL,
    org_id            TEXT NOT NULL,
    customer_id       TEXT NOT NULL,
    agent_id          TEXT NOT NULL,
    funding_method_id TEXT NOT NULL,
    terms             TEXT NOT NULL,
    terms_hash        TEXT NOT NULL CHECK (terms_hash = 'sha256:' || encode(sha256(convert_to(terms, 'UTF8')), 'hex')),
    expires_at        TIMESTAMPTZ NOT NULL,
    approved_at       TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ,
    FOREIGN KEY (merchant_id, org_id) REFERENCES merchant_integrations (id, org_id),
    FOREIGN KEY (customer_id, merchant_id, org_id) REFERENCES commerce_customers (id, merchant_id, org_id),
    FOREIGN KEY (agent_id, merchant_id, org_id) REFERENCES commerce_agents (id, merchant_id, org_id),
    FOREIGN KEY (funding_method_id, customer_id, merchant_id, org_id)
        REFERENCES commerce_funding_methods (id, customer_id, merchant_id, org_id)
);
CREATE INDEX commerce_mandates_list ON commerce_mandates (merchant_id, approved_at DESC, id DESC);
CREATE INDEX commerce_mandates_customer ON commerce_mandates (merchant_id, customer_id, approved_at DESC);
