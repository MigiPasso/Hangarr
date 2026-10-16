-- Last proof of the password in a session: the sign-in itself, or a
-- confirmation asked before sensitive operations (Settings › Services, whose
-- connection tests reach any address). Existing sessions have none: they
-- confirm once.
ALTER TABLE sessions ADD COLUMN reauth_at INTEGER;
