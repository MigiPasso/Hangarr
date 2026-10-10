-- Jellyfin as the media server, instead of Plex (one at a time). Each server
-- has its own key columns; the sync only rewrites those of the server it
-- reads. Items only the other server knows are kept, frozen and hidden
-- (match_state 'hidden'), with their history and local protection, until
-- that server is used again.
ALTER TABLE media ADD COLUMN jellyfin_id TEXT;
ALTER TABLE seasons ADD COLUMN jellyfin_id TEXT;
CREATE UNIQUE INDEX media_jellyfin_only ON media(kind, jellyfin_id) WHERE arr_id IS NULL;
CREATE INDEX media_jellyfin_id ON media(jellyfin_id);
-- match_state: matched | arr_only | server_only (was plex_only) | hidden.
UPDATE media SET match_state = 'server_only' WHERE match_state = 'plex_only';
