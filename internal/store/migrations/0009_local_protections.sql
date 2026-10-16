-- Protection kept by Hangarr only, bound to the item's identity instead of a
-- media row: external ids, media server keys, else normalized title and year.
-- It survives a new Plex rating key, a server switch and Sonarr/Radarr taking
-- the item over (the server_only row is then deleted). media.protected_local
-- becomes a cache of this table, refreshed after every catalog write.
CREATE TABLE local_protections (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,                -- movie | series
  tmdb_id INTEGER,
  tvdb_id INTEGER,
  imdb_id TEXT,
  plex_rating_key TEXT,
  jellyfin_id TEXT,
  title TEXT NOT NULL,
  title_norm TEXT NOT NULL DEFAULT '',
  year INTEGER,
  created_at INTEGER NOT NULL,
  username TEXT
);
CREATE INDEX local_protections_kind ON local_protections(kind);

INSERT INTO local_protections(kind, tmdb_id, tvdb_id, imdb_id, plex_rating_key, jellyfin_id, title, title_norm,
  year, created_at)
SELECT kind, tmdb_id, tvdb_id, imdb_id, plex_rating_key, jellyfin_id, title, title_norm, year,
  COALESCE(updated_at, 0)
FROM media WHERE protected_local;
