-- Every agent submission and every Airwallex call. Observability, not
-- the money-safety record (that is intent_events): written beside it.
CREATE TABLE attempts (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organisations(id),
    kind             TEXT NOT NULL,
    operation        TEXT NOT NULL,
    outcome          TEXT NOT NULL,
    intent_key       TEXT,
    beneficiary_key  TEXT,
    agent_id         TEXT REFERENCES agents(id),
    request_id       TEXT,
    transfer_id      TEXT,
    transfer_status  TEXT,
    http_status      INT,
    error_code       TEXT,
    error_message    TEXT,
    activity_attempt INT,
    details          JSONB NOT NULL DEFAULT '{}',
    started_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ
);
CREATE INDEX attempts_org_started ON attempts (org_id, started_at DESC, id DESC);
CREATE INDEX attempts_org_intent ON attempts (org_id, intent_key, started_at);
