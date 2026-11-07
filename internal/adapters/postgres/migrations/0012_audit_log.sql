-- Account actions: who did what, to what, from where. Payments have
-- their own history (intent_events, attempts); this is the people side.
CREATE TABLE audit_events (
    id            TEXT PRIMARY KEY,
    org_id        TEXT REFERENCES organisations(id),
    actor_user_id TEXT REFERENCES users(id),
    actor_email   TEXT,
    action        TEXT NOT NULL,
    target_type   TEXT,
    target_id     TEXT,
    details       JSONB NOT NULL DEFAULT '{}',
    ip            TEXT,
    user_agent    TEXT,
    at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX audit_events_org_at ON audit_events (org_id, at DESC, id DESC);
CREATE INDEX audit_events_actor ON audit_events (actor_user_id, at DESC);
