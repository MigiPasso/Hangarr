-- Lockout per account and client (IPv4 address or IPv6 /64), instead of per
-- account: failures sent by one client no longer lock the account for
-- everyone else, the right password included.
CREATE TABLE login_failures (
  user_id INTEGER NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
  client TEXT NOT NULL,
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  locked_until INTEGER,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, client)
);
-- app_users.failed_attempts now counts the failures of every client since
-- the last sign-in; that count is kept at sign-in for the Account page.
-- app_users.locked_until is no longer used.
ALTER TABLE app_users ADD COLUMN failures_before_login INTEGER NOT NULL DEFAULT 0;
UPDATE app_users SET locked_until = NULL;
