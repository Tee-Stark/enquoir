-- Merchant-delegated commerce, phases 6 and 7: what became of each
-- execution, its append-only history, and the PSP webhook events seen.

ALTER TABLE commerce_executions DROP CONSTRAINT commerce_executions_status_check;
ALTER TABLE commerce_executions
    ADD CONSTRAINT commerce_executions_status_check CHECK (status IN ('pending', 'succeeded', 'failed', 'unknown')),
    ADD COLUMN provider_payment_reference TEXT,
    ADD COLUMN failure_reason             TEXT,
    ADD COLUMN updated_at                 TIMESTAMPTZ,
    ADD COLUMN report_deadline            TIMESTAMPTZ,
    ADD COLUMN finished_at                TIMESTAMPTZ;
UPDATE commerce_executions SET updated_at = created_at, report_deadline = created_at + interval '30 minutes';
ALTER TABLE commerce_executions
    ALTER COLUMN updated_at SET NOT NULL,
    ALTER COLUMN report_deadline SET NOT NULL,
    ADD CONSTRAINT commerce_executions_finished CHECK ((status IN ('succeeded', 'failed')) = (finished_at IS NOT NULL));
-- The sweep's work: pending executions by deadline.
CREATE INDEX commerce_executions_overdue ON commerce_executions (report_deadline) WHERE status = 'pending';
CREATE INDEX commerce_executions_list ON commerce_executions (merchant_id, created_at DESC, id DESC);

ALTER TABLE commerce_checkouts DROP CONSTRAINT commerce_checkouts_status_check;
ALTER TABLE commerce_checkouts ADD CONSTRAINT commerce_checkouts_status_check
    CHECK (status IN ('authorized', 'awaiting_customer_approval', 'denied', 'rejected', 'canceled', 'redeemed', 'revoked', 'paid', 'payment_failed'));
CREATE INDEX commerce_checkouts_list ON commerce_checkouts (merchant_id, created_at DESC, id DESC);

-- Every report on an execution, applied or not, in order. Never changed.
CREATE TABLE commerce_execution_events (
    execution_id TEXT NOT NULL REFERENCES commerce_executions(id),
    sequence     INT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('redeemed', 'reported', 'duplicate', 'ignored', 'refused', 'swept')),
    source       TEXT,
    outcome      TEXT,
    status_after TEXT NOT NULL,
    reference    TEXT,
    event_id     TEXT,
    amount_minor BIGINT,
    currency     TEXT,
    detail       TEXT,
    at           TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (execution_id, sequence)
);
CREATE FUNCTION commerce_execution_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution history is append-only';
END $$;
CREATE TRIGGER commerce_execution_events_immutable
    BEFORE UPDATE OR DELETE ON commerce_execution_events
    FOR EACH ROW EXECUTE FUNCTION commerce_execution_events_immutable();
INSERT INTO commerce_execution_events (execution_id, sequence, kind, status_after, amount_minor, currency, at)
SELECT id, 1, 'redeemed', status, amount_minor, currency, created_at FROM commerce_executions;

-- PSP webhook event IDs already reported, per merchant and provider: a
-- redelivery is recognised and changes nothing.
CREATE TABLE commerce_webhook_events (
    merchant_id  TEXT NOT NULL REFERENCES merchant_integrations(id),
    provider     TEXT NOT NULL,
    event_id     TEXT NOT NULL,
    execution_id TEXT NOT NULL REFERENCES commerce_executions(id),
    fingerprint  TEXT NOT NULL,
    at           TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (merchant_id, provider, event_id)
);
