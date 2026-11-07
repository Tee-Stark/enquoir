-- The email outbox: what to send to whom, never the rendered message
-- and never a link. template is an ID of a template embedded in the
-- binary; params are non-secret values (an organisation name).
CREATE TABLE emails (
    id            TEXT PRIMARY KEY,
    org_id        TEXT REFERENCES organisations(id),
    user_id       TEXT REFERENCES users(id),
    to_email      TEXT NOT NULL,
    template      TEXT NOT NULL,
    params        JSONB NOT NULL DEFAULT '{}',
    link_purpose  TEXT,
    link_subject  TEXT,
    link_ttl_secs INT,
    status        TEXT NOT NULL CHECK (status IN ('queued', 'sent', 'failed')),
    provider_id   TEXT,
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL,
    sent_at       TIMESTAMPTZ
);
CREATE INDEX emails_queued ON emails (created_at) WHERE status = 'queued';
CREATE INDEX emails_user ON emails (user_id, created_at DESC);

-- One-time link tokens, by hash only. A token is minted when its email
-- is sent; subject is the user or invitation it acts on.
CREATE TABLE one_time_tokens (
    token_hash TEXT PRIMARY KEY,
    purpose    TEXT NOT NULL,
    subject    TEXT NOT NULL,
    email_id   TEXT REFERENCES emails(id),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX one_time_tokens_subject ON one_time_tokens (purpose, subject) WHERE used_at IS NULL;

ALTER TABLE users ADD COLUMN password_changed_at TIMESTAMPTZ;
