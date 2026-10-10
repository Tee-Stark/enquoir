CREATE TABLE intent_events (
    intent_key      TEXT NOT NULL REFERENCES intents(key),
    sequence_number INT NOT NULL,
    type            TEXT NOT NULL,
    payload         TEXT NOT NULL,
    prev_hash       TEXT NOT NULL,
    hash            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (intent_key, sequence_number)
);
