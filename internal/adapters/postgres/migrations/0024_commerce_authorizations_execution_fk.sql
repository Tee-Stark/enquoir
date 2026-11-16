-- commerce_executions.authorization_id already references
-- commerce_authorizations(id); the reverse pointer, set once a grant is
-- redeemed, had no FK (commerce_executions didn't exist yet when
-- commerce_authorizations was created in migration 0021). The
-- application always sets both sides in the same transaction, so this
-- was never reachable - but every other cross-table pointer in this
-- schema is FK'd, and a future direct write or migration bug deserves
-- the same backstop here.
ALTER TABLE commerce_authorizations
    ADD CONSTRAINT commerce_authorizations_execution_id_fkey
    FOREIGN KEY (execution_id) REFERENCES commerce_executions(id);
