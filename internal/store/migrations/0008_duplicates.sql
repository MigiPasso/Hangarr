-- Versions (files) of the movies a media server has several times: one item
-- with several versions, or several items with the same TMDB or IMDb id.
-- A cache of that server, replaced at each of its catalog syncs; the items
-- are found through media.plex_rating_key or media.jellyfin_id.
CREATE TABLE media_versions (
  server      TEXT NOT NULL,             -- plex | jellyfin
  version_key TEXT NOT NULL,             -- Plex Media id, Jellyfin media source id
  item_key    TEXT NOT NULL,             -- Plex ratingKey, Jellyfin item id
  group_key   TEXT NOT NULL,             -- tmdb:N, imdb:ttN or item:KEY
  file        TEXT NOT NULL DEFAULT '',  -- path of the first file
  parts       INTEGER NOT NULL DEFAULT 1,
  size_bytes  INTEGER NOT NULL DEFAULT 0,
  resolution  TEXT NOT NULL DEFAULT '',
  video_codec TEXT NOT NULL DEFAULT '',
  audio_codec TEXT NOT NULL DEFAULT '',
  container   TEXT NOT NULL DEFAULT '',
  bitrate     INTEGER NOT NULL DEFAULT 0, -- kbit/s
  PRIMARY KEY (server, version_key)
);
CREATE INDEX media_versions_group ON media_versions(server, group_key);
CREATE INDEX media_versions_item ON media_versions(server, item_key);

-- Duplicates kept on purpose, for one exact set of versions (fingerprint:
-- sorted version keys): a version added or removed shows the group again.
CREATE TABLE duplicate_acks (
  server      TEXT NOT NULL,
  group_key   TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  acked_at    INTEGER NOT NULL,
  PRIMARY KEY (server, group_key)
);
