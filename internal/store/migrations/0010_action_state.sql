-- Two-step journal: a deletion is written 'pending' before the call to
-- Sonarr/Radarr or the media server, and completed ('done') after it. A row
-- left pending by a stop (or a crash) is marked 'interrupted' at the next
-- start: whether the deletion happened must be checked upstream.
ALTER TABLE action_log ADD COLUMN state TEXT NOT NULL DEFAULT 'done';
-- Media server key of an item deleted through the server: a sync that read
-- the server before the deletion skips it.
ALTER TABLE action_log ADD COLUMN server_key TEXT;
