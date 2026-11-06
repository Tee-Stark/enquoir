-- Tenants are isolated (AGENTS.md): every intent and event belongs to
-- one organisation. Recorded history is the audit trail and is never
-- deleted: intents recorded before organisations existed move to the
-- "pre-tenancy" organisation (postgres.PreTenancyOrgID), hash chains
-- untouched. Their keys come from an older derivation, so a new
-- submission never matches them; they stay readable and verifiable.
INSERT INTO organisations (id, name, created_at)
SELECT 'org_pre_tenancy', 'Pre-tenancy history', now()
WHERE EXISTS (SELECT 1 FROM intents);

ALTER TABLE intents ADD COLUMN org_id TEXT REFERENCES organisations(id);
UPDATE intents SET org_id = 'org_pre_tenancy';
ALTER TABLE intents ALTER COLUMN org_id SET NOT NULL;
CREATE INDEX intents_org_id ON intents (org_id);

ALTER TABLE intent_events ADD COLUMN org_id TEXT REFERENCES organisations(id);
UPDATE intent_events SET org_id = 'org_pre_tenancy';
ALTER TABLE intent_events ALTER COLUMN org_id SET NOT NULL;
CREATE INDEX intent_events_org_id ON intent_events (org_id);
