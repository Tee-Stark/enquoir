-- Records which policy version decided an intent at submission, for
-- every outcome (Allow included, not just Deny/Escalate's audit
-- entries) - AGENTS.md's Phase 6 requires every decision to record
-- the version it used. Nullable: an intent submitted with no
-- Policies store configured, or recorded before this column existed,
-- has no known deciding version.
ALTER TABLE intents ADD COLUMN policy_version INT;
