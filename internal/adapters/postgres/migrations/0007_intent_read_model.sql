-- The read model the dashboard filters on: who submitted each intent,
-- and its current state, kept in step with the event log by
-- EventLog.Append in the same transaction as each event. The event log
-- stays the source of truth; state is NULL until the first event
-- (shown as RECEIVED).
ALTER TABLE intents
    ADD COLUMN agent_id         TEXT REFERENCES agents(id),
    ADD COLUMN state            TEXT,
    ADD COLUMN state_changed_at TIMESTAMPTZ,
    ADD COLUMN transfer_id      TEXT,
    ADD COLUMN failure_reason   TEXT;

UPDATE intents i
SET state = last.type,
    state_changed_at = last.created_at,
    transfer_id = CASE WHEN last.type = 'APPLIED' THEN last.payload END,
    failure_reason = CASE WHEN last.type IN ('FAILED', 'BLOCKED') THEN last.payload END
FROM (
    SELECT DISTINCT ON (intent_key) intent_key, type, payload, created_at
    FROM intent_events
    ORDER BY intent_key, sequence_number DESC
) last
WHERE last.intent_key = i.key;

CREATE INDEX intents_org_created ON intents (org_id, created_at DESC, key DESC);
CREATE INDEX intents_org_state ON intents (org_id, state);
