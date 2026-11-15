-- The dashboard's new org-wide commerce activity view (Payments/Activity
-- in Merchant mode) lists checkouts and executions scoped only by
-- org_id, across every merchant integration. The existing indexes are
-- merchant-scoped (commerce_checkouts_list/commerce_executions_list on
-- (merchant_id, created_at DESC, id DESC)), so an org-wide query would
-- otherwise scan the whole table - mirrors intents_org_created.
CREATE INDEX commerce_checkouts_org_created ON commerce_checkouts (org_id, created_at DESC, id DESC);
CREATE INDEX commerce_executions_org_created ON commerce_executions (org_id, created_at DESC, id DESC);
