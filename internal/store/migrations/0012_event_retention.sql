-- Repeated sign-in failures from the same client are stored as one row with
-- a count, and the security log is purged after a retention period: an
-- anonymous client no longer adds rows without bound.
ALTER TABLE auth_events ADD COLUMN repeat_count INTEGER NOT NULL DEFAULT 1;
CREATE INDEX auth_events_ip ON auth_events(ip, id);
