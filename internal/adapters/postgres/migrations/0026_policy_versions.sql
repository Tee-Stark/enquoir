-- Phase 6: one spending policy per organisation. Every edit is a new
-- version, never a mutation, so a past decision's recorded version
-- always reads back exactly what it was decided against - the
-- snapshot is stored as exact text, its hash checked, same discipline
-- as commerce's mandate terms.
CREATE TABLE policy_versions (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organisations(id),
    version    INT NOT NULL,
    canonical  TEXT NOT NULL,
    hash       TEXT NOT NULL CHECK (hash = 'sha256:' || encode(sha256(convert_to(canonical, 'UTF8')), 'hex')),
    created_at TIMESTAMPTZ NOT NULL,
    created_by TEXT REFERENCES users(id),
    UNIQUE (org_id, version)
);

-- A pure mutex row per organisation: evaluating a candidate payment
-- and saving a new policy version both lock their organisation's row
-- first, so they serialise against each other and against concurrent
-- evaluations. Kept separate from treasuries, which may become
-- many-per-organisation later. No organisation ever edited a policy
-- before this migration, so there is nothing to backfill in
-- policy_versions itself - an organisation with no row is evaluated
-- against policy.DefaultPolicyVersion (every limit unlimited).
CREATE TABLE organisation_policy_locks (
    org_id TEXT PRIMARY KEY REFERENCES organisations(id)
);
INSERT INTO organisation_policy_locks (org_id) SELECT id FROM organisations;
