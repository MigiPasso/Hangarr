-- Deletion journal (SPEC §5, §7.4) and data needed by the detail page.

CREATE TABLE action_log (
  id INTEGER PRIMARY KEY,
  at INTEGER NOT NULL,
  action TEXT NOT NULL,             -- delete_series | delete_season | delete_movie
  media_kind TEXT NOT NULL,
  arr_id INTEGER,
  title TEXT NOT NULL,
  season_number INTEGER,
  size_freed_bytes INTEGER NOT NULL DEFAULT 0,
  delete_files INTEGER NOT NULL,
  import_exclusion INTEGER NOT NULL,
  dry_run INTEGER NOT NULL,
  success INTEGER NOT NULL,
  error TEXT,
  username TEXT                     -- app user who requested it
);
CREATE INDEX action_log_at ON action_log(at);

-- Sonarr/Radarr web UI slug, for "open in Sonarr/Radarr" links.
ALTER TABLE media ADD COLUMN title_slug TEXT;
