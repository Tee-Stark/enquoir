-- Merchant-delegated commerce, phase 2: funding-method bindings. Each is
-- Enquoir's opaque reference to one saved payment method of one
-- customer. The PSP token stays with the merchant: no column here can
-- hold one, and none holds card data.

-- Lets a binding name its customer, merchant and organisation together,
-- so a binding can never point at another merchant's customer.
ALTER TABLE commerce_customers
    ADD CONSTRAINT commerce_customers_id_merchant_org UNIQUE (id, merchant_id, org_id);

CREATE TABLE commerce_funding_methods (
    id                         TEXT PRIMARY KEY,
    merchant_id                TEXT NOT NULL,
    org_id                     TEXT NOT NULL,
    customer_id                TEXT NOT NULL,
    merchant_funding_method_id TEXT NOT NULL,
    provider                   TEXT NOT NULL,
    display_brand              TEXT NOT NULL,
    -- Exactly four digits, or none: never more of a card than that.
    display_last_four          TEXT CHECK (display_last_four ~ '^[0-9]{4}$'),
    status                     TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at                 TIMESTAMPTZ NOT NULL,
    disabled_at                TIMESTAMPTZ,
    CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
    FOREIGN KEY (customer_id, merchant_id, org_id) REFERENCES commerce_customers (id, merchant_id, org_id),
    -- The merchant's namespace: one reference, one binding.
    UNIQUE (merchant_id, merchant_funding_method_id)
);
CREATE INDEX commerce_funding_methods_customer ON commerce_funding_methods (merchant_id, customer_id, created_at DESC, id DESC);
