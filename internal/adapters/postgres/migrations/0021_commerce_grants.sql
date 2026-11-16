-- Merchant-delegated commerce, phase 5: an authorized checkout's
-- authorization, its one-time grant (kept only as a hash), and the one
-- execution redeeming it creates. Every write touching a mandate's
-- checkouts or grants locks the mandate row first, so redemption,
-- cancellation, revocation and decisions are serialised per mandate.

ALTER TABLE commerce_checkouts DROP CONSTRAINT commerce_checkouts_status_check;
ALTER TABLE commerce_checkouts ADD CONSTRAINT commerce_checkouts_status_check
    CHECK (status IN ('authorized', 'awaiting_customer_approval', 'denied', 'rejected', 'canceled', 'redeemed', 'revoked'));

CREATE TABLE commerce_authorizations (
    id                   TEXT PRIMARY KEY,
    checkout_id          TEXT NOT NULL UNIQUE REFERENCES commerce_checkouts(id),
    mandate_id           TEXT NOT NULL,
    merchant_id          TEXT NOT NULL,
    org_id               TEXT NOT NULL,
    external_checkout_id TEXT NOT NULL,
    checkout_hash        TEXT NOT NULL,
    amount_minor         BIGINT NOT NULL CHECK (amount_minor > 0),
    currency             TEXT NOT NULL,
    funding_method_id    TEXT NOT NULL REFERENCES commerce_funding_methods(id),
    -- The current grant's hash; a reissue replaces it.
    grant_hash           TEXT UNIQUE,
    grant_issued_at      TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL,
    expires_at           TIMESTAMPTZ NOT NULL,
    redeemed_at          TIMESTAMPTZ,
    revoked_at           TIMESTAMPTZ,
    execution_id         TEXT,
    CHECK ((grant_hash IS NULL) = (grant_issued_at IS NULL)),
    CHECK ((redeemed_at IS NULL) = (execution_id IS NULL)),
    CHECK (redeemed_at IS NULL OR revoked_at IS NULL),
    FOREIGN KEY (mandate_id, merchant_id, org_id) REFERENCES commerce_mandates (id, merchant_id, org_id)
);
CREATE INDEX commerce_authorizations_open ON commerce_authorizations (mandate_id) WHERE redeemed_at IS NULL AND revoked_at IS NULL;

-- One execution per authorization: the payment a redeemed grant permits.
CREATE TABLE commerce_executions (
    id                   TEXT PRIMARY KEY,
    authorization_id     TEXT NOT NULL UNIQUE REFERENCES commerce_authorizations(id),
    checkout_id          TEXT NOT NULL REFERENCES commerce_checkouts(id),
    external_checkout_id TEXT NOT NULL,
    checkout_hash        TEXT NOT NULL,
    mandate_id           TEXT NOT NULL,
    merchant_id          TEXT NOT NULL,
    org_id               TEXT NOT NULL,
    amount_minor         BIGINT NOT NULL CHECK (amount_minor > 0),
    currency             TEXT NOT NULL,
    funding_method_id    TEXT NOT NULL REFERENCES commerce_funding_methods(id),
    status               TEXT NOT NULL CHECK (status IN ('pending')),
    created_at           TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (mandate_id, merchant_id, org_id) REFERENCES commerce_mandates (id, merchant_id, org_id)
);

-- Checkouts authorized before grants existed get an authorization that
-- expired 15 minutes after their decision, as one issued then would
-- have: they stop counting against their mandate.
INSERT INTO commerce_authorizations (id, checkout_id, mandate_id, merchant_id, org_id, external_checkout_id, checkout_hash,
    amount_minor, currency, funding_method_id, created_at, expires_at)
SELECT 'auth_' || id, id, mandate_id, merchant_id, org_id, external_id, snapshot_hash, total_minor, currency, funding_method_id,
       decided_at, decided_at + interval '15 minutes'
FROM commerce_checkouts WHERE status = 'authorized';
