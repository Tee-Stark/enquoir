-- Each person lists the events about their own account.
CREATE INDEX audit_events_target_user ON audit_events (target_id, at DESC) WHERE target_type = 'user';
