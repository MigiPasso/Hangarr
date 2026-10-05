package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is a row of app_users.
type User struct {
	ID                 int64
	Username           string
	PasswordHash       string
	MustChangePassword bool
	PasswordChangedAt  *int64
	FailedAttempts     int
	LockedUntil        *int64
	CreatedAt          int64
	LastLoginAt        *int64
}

// Session is a row of sessions joined with its user.
type Session struct {
	ID                 int64
	TokenHash          string
	UserID             int64
	Username           string
	MustChangePassword bool
	CSRFToken          string
	CreatedAt          int64
	LastSeenAt         int64
	ExpiresAt          int64
	IP                 string
	UserAgent          string
}

// AuthEvent is a row of auth_events.
type AuthEvent struct {
	At        int64
	UserID    *int64
	Username  string
	Event     string
	IP        string
	UserAgent string
}

const userColumns = `id, username, password_hash, must_change_password, password_changed_at,
	failed_attempts, locked_until, created_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var changed, locked, lastLogin sql.NullInt64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.MustChangePassword, &changed,
		&u.FailedAttempts, &locked, &u.CreatedAt, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.PasswordChangedAt, u.LockedUntil, u.LastLoginAt = ptrInt(changed), ptrInt(locked), ptrInt(lastLogin)
	return &u, nil
}

// CreateFirstUser inserts a user only if app_users is empty. The check and
// the insert share one IMMEDIATE transaction, and the UNIQUE constraint is a
// second safety net: concurrent starts on the same file create it once.
// The auth event is written in the same transaction.
func (s *Store) CreateFirstUser(ctx context.Context, username, passwordHash string, now time.Time) (created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO app_users(username, password_hash, must_change_password, created_at)
		VALUES (?, ?, 1, ?)`, username, passwordHash, now.Unix())
	if err != nil {
		return false, fmt.Errorf("insert admin: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_events(at, user_id, username, event) VALUES (?, ?, ?, 'admin_created')`,
		now.Unix(), id, username); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// UserByUsername looks up a user case-insensitively.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM app_users WHERE username = ?`, username))
}

// UserByID looks up a user by id.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM app_users WHERE id = ?`, id))
}

// SetLoginFailure stores the failure counter and optional lock deadline.
func (s *Store) SetLoginFailure(ctx context.Context, userID int64, failedAttempts int, lockedUntil *int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE app_users SET failed_attempts = ?, locked_until = ? WHERE id = ?`,
		failedAttempts, nullInt(lockedUntil), userID)
	return err
}

// SetLoginSuccess resets the failure counter, records the login time and,
// when newHash is not empty, replaces the hash (transparent rehash).
func (s *Store) SetLoginSuccess(ctx context.Context, userID int64, now time.Time, newHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE app_users SET failed_attempts = 0, locked_until = NULL, last_login_at = ?,
		password_hash = CASE WHEN ? <> '' THEN ? ELSE password_hash END WHERE id = ?`,
		now.Unix(), newHash, newHash, userID)
	return err
}

// ChangePassword stores a new hash chosen by the user, clears the
// must-change flag, deletes all the user's sessions and creates newSession,
// atomically.
func (s *Store) ChangePassword(ctx context.Context, userID int64, hash string, now time.Time, newSession *Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE app_users SET password_hash = ?, must_change_password = 0,
		password_changed_at = ?, failed_attempts = 0, locked_until = NULL WHERE id = ?`, hash, now.Unix(), userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if err := insertSession(ctx, tx, newSession); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetPassword is used by the CLI: new temporary hash, must-change flag,
// unlock and revoke every session, atomically.
func (s *Store) ResetPassword(ctx context.Context, userID int64, hash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE app_users SET password_hash = ?, must_change_password = 1,
		failed_attempts = 0, locked_until = NULL WHERE id = ?`, hash, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertSession(ctx context.Context, ex execer, sess *Session) error {
	res, err := ex.ExecContext(ctx, `INSERT INTO sessions(token_hash, user_id, csrf_token, created_at, last_seen_at,
		expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.UserID, sess.CSRFToken, sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt, sess.IP, sess.UserAgent)
	if err != nil {
		return err
	}
	sess.ID, err = res.LastInsertId()
	return err
}

// CreateSession inserts a session and sets its ID.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	return insertSession(ctx, s.db, sess)
}

const sessionColumns = `s.id, s.token_hash, s.user_id, u.username, u.must_change_password, s.csrf_token,
	s.created_at, s.last_seen_at, s.expires_at, COALESCE(s.ip, ''), COALESCE(s.user_agent, '')`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var x Session
	err := row.Scan(&x.ID, &x.TokenHash, &x.UserID, &x.Username, &x.MustChangePassword, &x.CSRFToken,
		&x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.IP, &x.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &x, err
}

// SessionByTokenHash returns the session and its user.
func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+`
		FROM sessions s JOIN app_users u ON u.id = s.user_id WHERE s.token_hash = ?`, tokenHash))
}

// ListSessions returns the user's sessions, most recently active first.
func (s *Store) ListSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+`
		FROM sessions s JOIN app_users u ON u.id = s.user_id WHERE s.user_id = ? ORDER BY s.last_seen_at DESC, s.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *x)
	}
	return out, rows.Err()
}

// TouchSession updates last_seen_at.
func (s *Store) TouchSession(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, now.Unix(), id)
	return err
}

// DeleteSession deletes one session of a user; it reports whether a row was deleted.
func (s *Store) DeleteSession(ctx context.Context, userID, sessionID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, sessionID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteExpiredSessions removes sessions past their absolute expiry or idle
// for longer than idleTimeout.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time, idleTimeout time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		now.Unix(), now.Add(-idleTimeout).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AddAuthEvent appends to auth_events.
func (s *Store) AddAuthEvent(ctx context.Context, e AuthEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_events(at, user_id, username, event, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`, e.At, nullInt(e.UserID), e.Username, e.Event, e.IP, e.UserAgent)
	return err
}

// AuthEvents returns the most recent auth events, newest first.
func (s *Store) AuthEvents(ctx context.Context, limit int) ([]AuthEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, user_id, COALESCE(username, ''), event, COALESCE(ip, ''),
		COALESCE(user_agent, '') FROM auth_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuthEvent
	for rows.Next() {
		var e AuthEvent
		var uid sql.NullInt64
		if err := rows.Scan(&e.At, &uid, &e.Username, &e.Event, &e.IP, &e.UserAgent); err != nil {
			return nil, err
		}
		e.UserID = ptrInt(uid)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Secret returns the named secret, creating it with gen() on first use.
func (s *Store) Secret(ctx context.Context, name string, gen func() ([]byte, error)) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_secrets WHERE name = ?`, name).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	fresh, err := gen()
	if err != nil {
		return nil, err
	}
	// Another process may have inserted it meanwhile: keep the first one.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO app_secrets(name, value, created_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO NOTHING`, name, fresh, time.Now().Unix()); err != nil {
		return nil, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT value FROM app_secrets WHERE name = ?`, name).Scan(&v)
	return v, err
}
