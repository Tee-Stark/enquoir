-- The org-wide commerce activity endpoints (0028) added indexes for the
-- unfiltered org_id+created_at scan, but customer_id/agent_id have no
-- supporting index at all: a filter value with few or no matches forces
-- Postgres to walk the org's entire checkout/execution history before it
-- can conclude the page is short. ListExecutionsForOrg's agent_id filter
-- is worse still - it joins to commerce_checkouts with no index on
-- commerce_executions.checkout_id to drive that join.
CREATE INDEX commerce_checkouts_org_customer ON commerce_checkouts (org_id, customer_id, created_at DESC, id DESC);
CREATE INDEX commerce_checkouts_org_agent ON commerce_checkouts (org_id, agent_id, created_at DESC, id DESC);
CREATE INDEX commerce_executions_checkout_id ON commerce_executions (checkout_id);
