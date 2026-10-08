// Package auth implements the application's login: first-start admin
// bootstrap, password policy and hashing, sessions, lockout and rate limits.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"hangarr/internal/store"
)

// Errors returned to the web layer.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrWrongPassword      = errors.New("current password is incorrect")
	ErrSessionInvalid     = errors.New("session invalid or expired")
	ErrUnknownUser        = errors.New("unknown user")
	// ErrWeakPassword: an operator-chosen password fails the policy.
	ErrWeakPassword = errors.New("password does not meet the policy (at least 12 characters " +
		"with lower and upper case letters, a digit and a symbol, without the username, not a common password)")
)

// Auth event names stored in auth_events.
const (
	EventAdminCreated     = "admin_created"
	EventLoginSuccess     = "login_success"
	EventLoginFailure     = "login_failure"
	EventLockout          = "lockout"
	EventPasswordChanged  = "password_changed"
	EventPasswordResetCLI = "password_reset_cli"
	EventLogout           = "logout"
	EventSessionRevoked   = "session_revoked"
)

// touchInterval bounds how often last_seen_at is written.
const touchInterval = time.Minute

// ClientMeta describes the client of a request, for sessions and events.
type ClientMeta struct {
	IP        string
	UserAgent string
}

// NewSession is a freshly created session; Token goes into the cookie and is
// never stored.
type NewSession struct {
	Token   string
	Session *store.Session
}

// Service orchestrates authentication on top of the store.
type Service struct {
	Store       *store.Store
	Hasher      *Hasher
	Log         *slog.Logger
	Now         func() time.Time
	IdleTimeout time.Duration
	MaxAge      time.Duration
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func truncate(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

func (s *Service) event(ctx context.Context, name string, userID *int64, username string, meta ClientMeta) {
	e := store.AuthEvent{
		At: s.now().Unix(), UserID: userID, Username: truncate(username, 64), Event: name,
		IP: meta.IP, UserAgent: truncate(meta.UserAgent, 256),
	}
	// A request canceled by the client must not prevent the audit trail.
	if err := s.Store.AddAuthEvent(context.WithoutCancel(ctx), e); err != nil {
		s.Log.Error("auth event not recorded", "event", name, "err", err)
	}
	level := slog.LevelInfo
	if name == EventLoginFailure || name == EventLockout {
		level = slog.LevelWarn
	}
	s.Log.Log(ctx, level, "auth event", "event", name, "username", e.Username, "ip", meta.IP)
}

// Banner returns the one-time admin password banner (SPEC §11.1).
func Banner(username, password string) string {
	line := strings.Repeat("=", 64)
	return fmt.Sprintf("%s\n HANGARR: INITIAL ADMIN ACCOUNT CREATED\n Username : %s\n Password : %s\n"+
		" This password will never be shown again.\n It must be changed at the first sign-in.\n%s\n",
		line, username, password, line)
}

// Bootstrap creates the admin account when app_users is empty and reports
// whether it was created. Without password, one is generated, written once to
// out and must be changed at the first sign-in. A password chosen by the
// operator (HANGARR_ADMIN_PASSWORD) must meet the policy, even when the
// account already exists; it is never written and need not be changed.
func (s *Service) Bootstrap(ctx context.Context, username, password string, out io.Writer) (bool, error) {
	chosen := password != ""
	if chosen {
		if v := CheckPolicy(password, username); len(v) > 0 {
			names := make([]string, len(v))
			for i, x := range v {
				names[i] = string(x)
			}
			return false, fmt.Errorf("%w: %s", ErrWeakPassword, strings.Join(names, ", "))
		}
	} else {
		var err error
		if password, err = GeneratePassword(GeneratedPasswordLength, username); err != nil {
			return false, err
		}
	}
	hash, err := s.Hasher.Hash(ctx, password)
	if err != nil {
		return false, err
	}
	created, err := s.Store.CreateFirstUser(ctx, username, hash, !chosen, s.now())
	if err != nil || !created {
		return false, err
	}
	if chosen {
		s.Log.Info("auth event", "event", EventAdminCreated, "username", username, "password", "HANGARR_ADMIN_PASSWORD")
		return true, nil
	}
	if _, err := io.WriteString(out, Banner(username, password)); err != nil {
		return true, err
	}
	s.Log.Info("auth event", "event", EventAdminCreated, "username", username, "password", "generated")
	return true, nil
}

// ResetPassword is the CLI recovery: new temporary password printed on out
// only, must-change flag, unlock and revocation of every session.
func (s *Service) ResetPassword(ctx context.Context, username string, out io.Writer) error {
	u, err := s.Store.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %q", ErrUnknownUser, username)
	}
	if err != nil {
		return err
	}
	pw, err := GeneratePassword(GeneratedPasswordLength, u.Username)
	if err != nil {
		return err
	}
	hash, err := s.Hasher.Hash(ctx, pw)
	if err != nil {
		return err
	}
	if err := s.Store.ResetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	s.event(ctx, EventPasswordResetCLI, &u.ID, u.Username, ClientMeta{IP: "cli"})
	_, err = fmt.Fprintf(out, "New temporary password for %q: %s\n"+
		"It must be changed at the next sign-in. Every session of this account was revoked.\n",
		u.Username, pw)
	return err
}

func (s *Service) newSession(ctx context.Context, userID int64, username string, mustChange bool, meta ClientMeta) (*NewSession, error) {
	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	csrf, err := NewToken()
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	sess := &store.Session{
		TokenHash: HashToken(token), UserID: userID, Username: username, MustChangePassword: mustChange,
		CSRFToken: csrf, CreatedAt: now, LastSeenAt: now, ExpiresAt: now + int64(s.MaxAge/time.Second),
		IP: meta.IP, UserAgent: truncate(meta.UserAgent, 256),
	}
	return &NewSession{Token: token, Session: sess}, nil
}

// Login checks credentials and opens a new session. Every failure returns
// ErrInvalidCredentials, whatever the reason, after the same hashing work.
func (s *Service) Login(ctx context.Context, username, password string, meta ClientMeta) (*NewSession, error) {
	username = strings.TrimSpace(username)
	if username == "" || utf8.RuneCountInString(password) > MaxPasswordLength*4 {
		s.Hasher.VerifyDummy(ctx, password)
		s.event(ctx, EventLoginFailure, nil, username, meta)
		return nil, ErrInvalidCredentials
	}
	u, err := s.Store.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		s.Hasher.VerifyDummy(ctx, password)
		s.event(ctx, EventLoginFailure, nil, username, meta)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if u.LockedUntil != nil && now.Unix() < *u.LockedUntil {
		s.Hasher.VerifyDummy(ctx, password)
		s.event(ctx, EventLoginFailure, &u.ID, u.Username, meta)
		return nil, ErrInvalidCredentials
	}
	ok, rehash, err := s.Hasher.Verify(ctx, password, u.PasswordHash)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		s.Log.Error("stored password hash is unreadable", "user_id", u.ID, "err", err)
	}
	if !ok {
		n := u.FailedAttempts + 1
		var lockedUntil *int64
		d := LockDuration(n)
		if d > 0 {
			t := now.Add(d).Unix()
			lockedUntil = &t
		}
		if err := s.Store.SetLoginFailure(ctx, u.ID, n, lockedUntil); err != nil {
			return nil, err
		}
		s.event(ctx, EventLoginFailure, &u.ID, u.Username, meta)
		if d > 0 {
			s.event(ctx, EventLockout, &u.ID, u.Username, meta)
		}
		return nil, ErrInvalidCredentials
	}

	newHash := ""
	if rehash {
		if newHash, err = s.Hasher.Hash(ctx, password); err != nil {
			return nil, err
		}
	}
	if err := s.Store.SetLoginSuccess(ctx, u.ID, now, newHash); err != nil {
		return nil, err
	}
	ns, err := s.newSession(ctx, u.ID, u.Username, u.MustChangePassword, meta)
	if err != nil {
		return nil, err
	}
	if err := s.Store.CreateSession(ctx, ns.Session); err != nil {
		return nil, err
	}
	s.event(ctx, EventLoginSuccess, &u.ID, u.Username, meta)
	return ns, nil
}

// Authenticate resolves a session cookie, enforcing idle and absolute expiry.
func (s *Service) Authenticate(ctx context.Context, token string) (*store.Session, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	sess, err := s.Store.SessionByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	idle := now.Sub(time.Unix(sess.LastSeenAt, 0))
	if now.Unix() >= sess.ExpiresAt || idle >= s.IdleTimeout {
		if _, err := s.Store.DeleteSession(ctx, sess.UserID, sess.ID); err != nil {
			return nil, err
		}
		return nil, ErrSessionInvalid
	}
	if idle >= touchInterval {
		if err := s.Store.TouchSession(ctx, sess.ID, now); err != nil {
			return nil, err
		}
		sess.LastSeenAt = now.Unix()
	}
	return sess, nil
}

// ChangePassword validates and stores a new password. On success every
// session of the user is revoked and a new one is returned (rotation).
// Violations are returned without error when the policy is not met.
func (s *Service) ChangePassword(ctx context.Context, sess *store.Session, current, next, confirm string, meta ClientMeta) (*NewSession, []Violation, error) {
	u, err := s.Store.UserByID(ctx, sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	ok, _, err := s.Hasher.Verify(ctx, current, u.PasswordHash)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, ErrWrongPassword
	}
	v := CheckPolicy(next, u.Username)
	if NormalizePassword(next) == NormalizePassword(current) {
		v = append(v, ViolationSameAsCurrent)
	}
	if NormalizePassword(next) != NormalizePassword(confirm) {
		v = append(v, ViolationMismatch)
	}
	if len(v) > 0 {
		return nil, v, nil
	}
	hash, err := s.Hasher.Hash(ctx, next)
	if err != nil {
		return nil, nil, err
	}
	ns, err := s.newSession(ctx, u.ID, u.Username, false, meta)
	if err != nil {
		return nil, nil, err
	}
	if err := s.Store.ChangePassword(ctx, u.ID, hash, s.now(), ns.Session); err != nil {
		return nil, nil, err
	}
	s.event(ctx, EventPasswordChanged, &u.ID, u.Username, meta)
	return ns, nil, nil
}

// Logout deletes the current session.
func (s *Service) Logout(ctx context.Context, sess *store.Session, meta ClientMeta) error {
	if _, err := s.Store.DeleteSession(ctx, sess.UserID, sess.ID); err != nil {
		return err
	}
	s.event(ctx, EventLogout, &sess.UserID, sess.Username, meta)
	return nil
}

// RevokeSession deletes one of the user's sessions by id.
func (s *Service) RevokeSession(ctx context.Context, sess *store.Session, sessionID int64, meta ClientMeta) (bool, error) {
	ok, err := s.Store.DeleteSession(ctx, sess.UserID, sessionID)
	if err != nil || !ok {
		return false, err
	}
	s.event(ctx, EventSessionRevoked, &sess.UserID, sess.Username, meta)
	return true, nil
}

// Sessions lists the user's active sessions.
func (s *Service) Sessions(ctx context.Context, userID int64) ([]store.Session, error) {
	all, err := s.Store.ListSessions(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := all[:0]
	for _, x := range all {
		if now.Unix() < x.ExpiresAt && now.Sub(time.Unix(x.LastSeenAt, 0)) < s.IdleTimeout {
			out = append(out, x)
		}
	}
	return out, nil
}

// PurgeExpired deletes expired sessions.
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	return s.Store.DeleteExpiredSessions(ctx, s.now(), s.IdleTimeout)
}
