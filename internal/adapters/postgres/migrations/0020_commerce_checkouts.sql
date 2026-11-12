-- Merchant-delegated commerce, phase 4: checkouts evaluated against a
-- mandate. Decisions that count against a mandate are taken under a
-- row lock on it (SELECT ... FOR UPDATE), so concurrent checkouts can't
-- overspend it. The snapshot is stored as exact text, its hash checked.

ALTER TABLE commerce_mandates
    ADD CONSTRAINT commerce_mandates_id_merchant_org UNIQUE (id, merchant_id, org_id);

CREATE TABLE commerce_checkouts (
    id                  TEXT PRIMARY KEY,
    merchant_id         TEXT NOT NULL,
    org_id              TEXT NOT NULL,
    mandate_id          TEXT NOT NULL,
    external_id         TEXT NOT NULL,
    customer_id         TEXT NOT NULL,
    agent_id            TEXT NOT NULL,
    funding_method_id   TEXT NOT NULL,
    currency            TEXT NOT NULL,
    total_minor         BIGINT NOT NULL CHECK (total_minor > 0),
    snapshot            TEXT NOT NULL,
    snapshot_hash       TEXT NOT NULL CHECK (snapshot_hash = 'sha256:' || encode(sha256(convert_to(snapshot, 'UTF8')), 'hex')),
    status              TEXT NOT NULL CHECK (status IN ('authorized', 'awaiting_customer_approval', 'denied', 'rejected', 'canceled')),
    denials             JSONB NOT NULL DEFAULT '[]',
    created_at          TIMESTAMPTZ NOT NULL,
    decided_at          TIMESTAMPTZ,
    approval_expires_at TIMESTAMPTZ,
    canceled_at         TIMESTAMPTZ,
    CHECK ((status = 'awaiting_customer_approval') <= (approval_expires_at IS NOT NULL)),
    CHECK ((status = 'canceled') = (canceled_at IS NOT NULL)),
    CHECK ((status = 'denied') = (denials <> '[]'::jsonb)),
    FOREIGN KEY (mandate_id, merchant_id, org_id) REFERENCES commerce_mandates (id, merchant_id, org_id),
    UNIQUE (merchant_id, external_id)
);
-- What a mandate has authorized, read under its lock on every decision.
CREATE INDEX commerce_checkouts_authorized ON commerce_checkouts (mandate_id) WHERE status = 'authorized';
