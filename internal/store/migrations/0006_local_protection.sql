-- Protection kept by Cleanarr only, for items Sonarr/Radarr do not manage
-- (plex_only): nothing is sent to Plex. The sync never writes this column.
ALTER TABLE media ADD COLUMN protected_local INTEGER NOT NULL DEFAULT 0;
