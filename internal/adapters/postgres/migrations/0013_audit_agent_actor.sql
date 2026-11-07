-- When an agent acts (registering a payee with its key), the audit log
-- names the agent.
ALTER TABLE audit_events
    ADD COLUMN actor_agent_id   TEXT REFERENCES agents(id),
    ADD COLUMN actor_agent_name TEXT;
