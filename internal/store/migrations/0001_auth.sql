-- Application authentication (SPEC §11).

CREATE TABLE app_users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,              -- PHC argon2id string
  must_change_password INTEGER NOT NULL DEFAULT 1,
  password_changed_at INTEGER,
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  locked_until INTEGER,
  created_at INTEGER NOT NULL,
  last_login_at INTEGER
);

-- id is a surrogate key used to revoke a session from the UI without ever
-- exposing token_hash in HTML.
CREATE TABLE sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT NOT NULL UNIQUE,          -- SHA-256 of the token, never the token itself
  user_id INTEGER NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
  csrf_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ip TEXT,
  user_agent TEXT
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE auth_events (
  id INTEGER PRIMARY KEY,
  at INTEGER NOT NULL,
  user_id INTEGER,
  username TEXT,
  event TEXT NOT NULL,                      -- admin_created | login_success | login_failure | lockout
                                            -- | password_changed | password_reset_cli | logout | session_revoked
  ip TEXT,
  user_agent TEXT
);
CREATE INDEX auth_events_at ON auth_events(at);

-- Server-side secrets generated on first start (e.g. pre-session CSRF HMAC key).
CREATE TABLE app_secrets (
  name TEXT PRIMARY KEY,
  value BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
