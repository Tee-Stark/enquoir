-- Which organisation registered which payee. Every organisation pays
-- through Enquoir's one rail account, so a rail beneficiary belongs to
-- exactly one organisation: rail_id is unique, never shared.
CREATE TABLE beneficiaries (
    key              TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organisations(id),
    rail_id          TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    account_last4    TEXT NOT NULL,
    routing_number   TEXT NOT NULL,
    account_category TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX beneficiaries_org_id ON beneficiaries (org_id, created_at);
