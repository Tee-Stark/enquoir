-- Temporal's auto-setup creates its schema but expects the databases to
-- already exist when the owning user lacks CREATEDB; the app's own
-- database comes from POSTGRES_DB and needs no extra step.
CREATE DATABASE temporal;
CREATE DATABASE temporal_visibility;
