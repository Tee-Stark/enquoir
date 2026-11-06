-- Tenants are isolated (AGENTS.md): every intent and event belongs to
-- one organisation, enforced explicitly here even though the intent
-- key already embeds one. The key's derivation changed to include the
-- organisation in the same change that added this column, so any
-- intent recorded before it is already a dead key under the new
-- derivation - there is nothing worth preserving across that break.
TRUNCATE TABLE intent_events, intents;

ALTER TABLE intents ADD COLUMN org_id TEXT NOT NULL REFERENCES organisations(id);
CREATE INDEX intents_org_id ON intents (org_id);

ALTER TABLE intent_events ADD COLUMN org_id TEXT NOT NULL REFERENCES organisations(id);
CREATE INDEX intent_events_org_id ON intent_events (org_id);
