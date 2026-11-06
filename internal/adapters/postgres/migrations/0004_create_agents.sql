CREATE TABLE agents (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organisations(id),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX agents_org_id ON agents (org_id, created_at);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL REFERENCES agents(id),
    org_id       TEXT NOT NULL REFERENCES organisations(id),
    prefix       TEXT NOT NULL,
    secret_hash  TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
-- At most one active key per agent.
CREATE UNIQUE INDEX api_keys_one_active_per_agent ON api_keys (agent_id) WHERE revoked_at IS NULL;
