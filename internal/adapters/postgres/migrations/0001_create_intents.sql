CREATE TABLE intents (
    key         TEXT PRIMARY KEY,
    payee       TEXT NOT NULL,
    minor_units BIGINT NOT NULL,
    currency    TEXT NOT NULL,
    reference   TEXT NOT NULL,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
