-- Catalog, watch history and sync bookkeeping (SPEC §5).

CREATE TABLE media (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,                       -- 'movie' | 'series'
  arr_id INTEGER,                           -- Sonarr/Radarr id, NULL for plex_only
  plex_rating_key TEXT,
  title TEXT NOT NULL,
  sort_title TEXT,
  year INTEGER,
  overview TEXT,
  tvdb_id INTEGER,
  tmdb_id INTEGER,
  imdb_id TEXT,
  status TEXT,                              -- continuing/ended (series)
  genres TEXT NOT NULL DEFAULT '[]',        -- JSON array
  network_or_studio TEXT,
  rating REAL,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  added_at INTEGER,                         -- unix
  tags TEXT NOT NULL DEFAULT '[]',          -- JSON array of labels
  quality_profile TEXT,
  monitored INTEGER,
  path TEXT,
  has_file INTEGER,
  total_episodes INTEGER,
  downloaded_episodes INTEGER,
  watched_episodes INTEGER,
  runtime_minutes INTEGER,
  poster_remote_url TEXT,
  fanart_remote_url TEXT,
  match_state TEXT NOT NULL,                -- matched | arr_only | plex_only
  match_method TEXT,                        -- tvdb | tmdb | imdb | fuzzy
  -- denormalized, recomputed after every sync:
  play_count INTEGER NOT NULL DEFAULT 0,
  completed_play_count INTEGER NOT NULL DEFAULT 0,
  watch_seconds INTEGER NOT NULL DEFAULT 0,
  distinct_viewers INTEGER NOT NULL DEFAULT 0,
  last_viewed_at INTEGER,
  last_viewed_by INTEGER,
  watch_state TEXT NOT NULL DEFAULT 'never', -- never | in_progress | completed | abandoned
  cleanup_score REAL,
  updated_at INTEGER,
  UNIQUE(kind, arr_id)
);
-- plex_only rows have no arr_id: they are keyed by their Plex rating key.
CREATE UNIQUE INDEX media_plex_only ON media(kind, plex_rating_key) WHERE arr_id IS NULL;
CREATE INDEX media_kind_state ON media(kind, watch_state);
CREATE INDEX media_last_viewed ON media(last_viewed_at);
CREATE INDEX media_size ON media(size_bytes);
CREATE INDEX media_plex_key ON media(plex_rating_key);

CREATE TABLE seasons (
  media_id INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  season_number INTEGER NOT NULL,
  plex_rating_key TEXT,
  monitored INTEGER,
  episode_file_count INTEGER NOT NULL DEFAULT 0,
  total_episode_count INTEGER NOT NULL DEFAULT 0,
  watched_episodes INTEGER NOT NULL DEFAULT 0,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  last_viewed_at INTEGER,
  PRIMARY KEY (media_id, season_number)
);

-- One row = one raw session (Tautulli grouping=0) or one Plex history entry.
CREATE TABLE watch_events (
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,                     -- 'tautulli' | 'plex'
  source_id INTEGER NOT NULL,
  reference_id INTEGER,                     -- groups resumed sessions into one play
  media_id INTEGER REFERENCES media(id) ON DELETE SET NULL,
  media_type TEXT,                          -- movie | episode
  rating_key TEXT,
  parent_rating_key TEXT,
  grandparent_rating_key TEXT,
  season_number INTEGER,
  episode_number INTEGER,
  user_id INTEGER,
  started_at INTEGER,
  stopped_at INTEGER,
  play_seconds INTEGER,
  percent_complete INTEGER,
  watched INTEGER NOT NULL DEFAULT 0,
  platform TEXT,
  player TEXT,
  transcode_decision TEXT,
  title TEXT,
  grandparent_title TEXT,
  year INTEGER,
  UNIQUE(source, source_id)
);
CREATE INDEX watch_events_stopped ON watch_events(stopped_at);
CREATE INDEX watch_events_media ON watch_events(media_id);
CREATE INDEX watch_events_user_media ON watch_events(user_id, media_id);
CREATE INDEX watch_events_reference ON watch_events(reference_id);

CREATE TABLE plex_accounts (
  account_id INTEGER PRIMARY KEY,
  username TEXT,
  friendly_name TEXT,
  is_active INTEGER,
  thumb_url TEXT
);

CREATE TABLE sync_cursors (
  source TEXT PRIMARY KEY,
  cursor TEXT,
  updated_at INTEGER
);

CREATE TABLE sync_runs (
  id INTEGER PRIMARY KEY,
  started_at INTEGER NOT NULL,
  finished_at INTEGER,
  source TEXT NOT NULL,
  status TEXT NOT NULL,                     -- running | success | error
  items INTEGER,
  error TEXT
);
CREATE INDEX sync_runs_source ON sync_runs(source, started_at);
