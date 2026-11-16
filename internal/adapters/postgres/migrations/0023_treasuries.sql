-- Every organisation has its own logical treasury: an attribution of
-- payments and payees over the one shared Airwallex account, never a
-- segregated provider balance. One per organisation for now (the unique
-- index goes when organisations may hold several).
CREATE TABLE treasuries (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organisations(id),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (id, org_id)
);
CREATE UNIQUE INDEX treasuries_one_per_org ON treasuries (org_id);

INSERT INTO treasuries (id, org_id, name, created_at)
SELECT 'trs_' || substr(md5(id), 1, 24), id, 'Main treasury', created_at FROM organisations;

-- Payments and payees belong to a treasury of their organisation. Rows
-- from before keep their keys (computed without a treasury); the app
-- finds them by those keys rather than deriving new ones.
ALTER TABLE intents ADD COLUMN treasury_id TEXT;
UPDATE intents i SET treasury_id = t.id FROM treasuries t WHERE t.org_id = i.org_id;
ALTER TABLE intents ALTER COLUMN treasury_id SET NOT NULL,
    ADD CONSTRAINT intents_treasury FOREIGN KEY (treasury_id, org_id) REFERENCES treasuries (id, org_id);

ALTER TABLE beneficiaries ADD COLUMN treasury_id TEXT;
UPDATE beneficiaries b SET treasury_id = t.id FROM treasuries t WHERE t.org_id = b.org_id;
ALTER TABLE beneficiaries ALTER COLUMN treasury_id SET NOT NULL,
    ADD CONSTRAINT beneficiaries_treasury FOREIGN KEY (treasury_id, org_id) REFERENCES treasuries (id, org_id);
