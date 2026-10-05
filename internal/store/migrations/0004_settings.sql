-- Settings changed from the UI (e.g. daily sync time), overriding env defaults.
CREATE TABLE app_settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
