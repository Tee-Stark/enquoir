-- One person, several organisations: a membership per (organisation,
-- user) with a role. Every existing user becomes the owner of the
-- organisation they belonged to.
CREATE TABLE memberships (
    org_id     TEXT NOT NULL REFERENCES organisations(id),
    user_id    TEXT NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'operator', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id, created_at);

INSERT INTO memberships (org_id, user_id, role, created_at)
SELECT org_id, id, 'owner', created_at FROM users;

ALTER TABLE users DROP COLUMN org_id, DROP COLUMN role;

-- Sessions get an ID, to be listed and revoked by; the token is never
-- stored.
ALTER TABLE sessions ADD COLUMN id TEXT;
UPDATE sessions SET id = 'ses_' || substr(token_hash, 1, 24);
ALTER TABLE sessions ALTER COLUMN id SET NOT NULL;
CREATE UNIQUE INDEX sessions_id ON sessions (id);
CREATE INDEX sessions_user_org ON sessions (user_id, org_id) WHERE revoked_at IS NULL;
