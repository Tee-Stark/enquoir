-- Records which member created an agent, so an escalation approval can
-- exclude them from approving their own agent's payment. Nullable:
-- agents created before this column existed have no known creator.
ALTER TABLE agents ADD COLUMN created_by TEXT REFERENCES users(id);
