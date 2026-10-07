package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hangarr/internal/store"
)

// testParams keep argon2id cheap in tests; production params are checked separately.
var testParams = HashParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestPolicyRules(t *testing.T) {
	cases := []struct {
		pw   string
		want []Violation
	}{
		{"Short1!", []Violation{ViolationTooShort}},
		{strings.Repeat("aA1!", 33), []Violation{ViolationTooLong}},
		{"ALLUPPERCASE123!", []Violation{ViolationNoLower}},
		{"alllowercase123!", []Violation{ViolationNoUpper}},
		{"NoDigitsHere!!!", []Violation{ViolationNoDigit}},
		{"NoSpecials12345", []Violation{ViolationNoSpecial}},
		{"MyAdminPass123!", []Violation{ViolationContainsUsername}},
		{"Valid-Passw0rd-42", nil},
		{"Ünïcödé-Pässwörd-9", nil},
		{"Espace autorisé 1A", nil},
	}
	for _, c := range cases {
		got := CheckPolicy(c.pw, "admin")
		if !slices.Equal(got, c.want) {
			t.Errorf("CheckPolicy(%q) = %v, want %v", c.pw, got, c.want)
		}
	}
}

func TestPolicyNFKC(t *testing.T) {
	// Fullwidth characters normalize to ASCII: "ａｄｍｉｎ" contains the username.
	if v := CheckPolicy("Ｘｙａｄｍｉｎ12345!", "admin"); !slices.Contains(v, ViolationContainsUsername) {
		t.Errorf("NFKC not applied before username check: %v", v)
	}
	// Length is counted in code points after normalization (ligature ﬁ -> fi).
	if NormalizePassword("ﬁ") != "fi" {
		t.Error("NFKC normalization missing")
	}
}

func TestCommonPasswords(t *testing.T) {
	if len(commonPasswords()) < 5000 {
		t.Fatalf("embedded common password list looks too small: %d", len(commonPasswords()))
	}
	for _, pw := range []string{"password", "PASSWORD", "123456", "qwerty"} {
		if !IsCommonPassword(pw) {
			t.Errorf("%q should be common", pw)
		}
	}
	if IsCommonPassword("Valid-Passw0rd-42") {
		t.Error("random password flagged as common")
	}
}

func TestGeneratePassword(t *testing.T) {
	ambiguous := regexp.MustCompile(`[0O1lI"'\\$` + "`" + `]`)
	for range 200 {
		pw, err := GeneratePassword(GeneratedPasswordLength, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != GeneratedPasswordLength {
			t.Fatalf("length %d", len(pw))
		}
		if ambiguous.MatchString(pw) {
			t.Fatalf("ambiguous character in %q", pw)
		}
		if v := CheckPolicy(pw, "admin"); len(v) != 0 {
			t.Fatalf("generated %q violates %v", pw, v)
		}
	}
}

func TestHashFormatAndVerify(t *testing.T) {
	h, err := NewHasher(testParams)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	enc, err := h.Hash(ctx, "Valid-Passw0rd-42")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=64,t=1,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(enc) {
		t.Fatalf("unexpected PHC string %q", enc)
	}
	ok, rehash, err := h.Verify(ctx, "Valid-Passw0rd-42", enc)
	if err != nil || !ok || rehash {
		t.Fatalf("verify: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(ctx, "wrong", enc); ok {
		t.Fatal("wrong password accepted")
	}
	if _, _, err := h.Verify(ctx, "x", "$argon2i$v=19$m=1,t=1,p=1$aa$bb"); err == nil {
		t.Fatal("argon2i hash must be rejected")
	}
}

func TestHashNormalizesInput(t *testing.T) {
	h, _ := NewHasher(testParams)
	ctx := context.Background()
	enc, _ := h.Hash(ctx, "Café-Passw0rd!")           // precomposed é
	ok, _, _ := h.Verify(ctx, "Café-Passw0rd!", enc) // e + combining acute
	if !ok {
		t.Fatal("NFKC-equivalent passwords must verify")
	}
}

func TestRehashWhenParamsChange(t *testing.T) {
	ctx := context.Background()
	old, _ := NewHasher(testParams)
	enc, _ := old.Hash(ctx, "Valid-Passw0rd-42")
	stronger := testParams
	stronger.Time = 2
	h, _ := NewHasher(stronger)
	ok, rehash, err := h.Verify(ctx, "Valid-Passw0rd-42", enc)
	if err != nil || !ok || !rehash {
		t.Fatalf("expected ok and rehash, got ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestDefaultParams(t *testing.T) {
	want := HashParams{Memory: 65536, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}
	if DefaultHashParams != want {
		t.Fatalf("DefaultHashParams = %+v", DefaultHashParams)
	}
	if testing.Short() {
		t.Skip("production-cost hash skipped in -short")
	}
	h := &Hasher{params: DefaultHashParams, sem: make(chan struct{}, 1)}
	enc, err := h.Hash(context.Background(), "Valid-Passw0rd-42")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected prefix: %s", enc)
	}
}

func TestLockDuration(t *testing.T) {
	want := map[int]time.Duration{
		0: 0, 4: 0, 5: time.Minute, 6: 5 * time.Minute, 7: 15 * time.Minute, 8: time.Hour, 20: time.Hour,
	}
	for n, d := range want {
		if got := LockDuration(n); got != d {
			t.Errorf("LockDuration(%d) = %s, want %s", n, got, d)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	rl := NewRateLimiter(3, 15*time.Minute, func() time.Time { return now })
	for i := range 3 {
		if ok, _ := rl.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d rejected", i)
		}
	}
	ok, retry := rl.Allow("1.2.3.4")
	if ok || retry != 15*time.Minute {
		t.Fatalf("4th attempt: ok=%v retry=%s", ok, retry)
	}
	if ok, _ := rl.Allow("5.6.7.8"); !ok {
		t.Fatal("other IPs must not be affected")
	}
	now = now.Add(15*time.Minute + time.Second)
	if ok, _ := rl.Allow("1.2.3.4"); !ok {
		t.Fatal("window must slide")
	}
	rl.Sweep()
}

func TestClientIP(t *testing.T) {
	r := IPResolver{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	cases := []struct {
		remote, xff, want string
	}{
		{"203.0.113.9:1234", "1.1.1.1", "203.0.113.9"},             // untrusted peer: XFF ignored
		{"10.0.0.2:1234", "", "10.0.0.2"},                          // trusted peer, no XFF
		{"10.0.0.2:1234", "1.1.1.1", "1.1.1.1"},                    // single hop
		{"10.0.0.2:1234", "6.6.6.6, 1.1.1.1, 10.0.0.3", "1.1.1.1"}, // spoofed left part skipped
		{"10.0.0.2:1234", "10.1.1.1, 10.0.0.3", "10.1.1.1"},        // all trusted: leftmost
		{"10.0.0.2:1234", "garbage, 1.1.1.1", "1.1.1.1"},
		{"10.0.0.2:1234", "1.1.1.1, garbage", "10.0.0.2"},
		{"[::ffff:10.0.0.2]:1234", "2001:db8::1", "2001:db8::1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := r.ClientIP(req).String(); got != c.want {
			t.Errorf("remote=%s xff=%q: got %s want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestPreSessionCSRF(t *testing.T) {
	key := []byte("k")
	tok := PreSessionCSRF(key, "cookie")
	if !CheckPreSessionCSRF(key, "cookie", tok) {
		t.Fatal("valid token rejected")
	}
	if CheckPreSessionCSRF(key, "other", tok) || CheckPreSessionCSRF([]byte("x"), "cookie", tok) ||
		CheckPreSessionCSRF(key, "", "") {
		t.Fatal("invalid token accepted")
	}
}

// --- service tests on a real SQLite database -------------------------------

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newService(t *testing.T) (*Service, *clock) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h, err := NewHasher(testParams)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	return &Service{
		Store: st, Hasher: h, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: clk.Now, IdleTimeout: 12 * time.Hour, MaxAge: 168 * time.Hour,
	}, clk
}

var bannerPassword = regexp.MustCompile(`(?m)^ Password : (\S+)$`)

func bootstrap(t *testing.T, s *Service) string {
	t.Helper()
	var out bytes.Buffer
	created, err := s.Bootstrap(context.Background(), "admin", &out)
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	m := bannerPassword.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no password in banner:\n%s", out.String())
	}
	return m[1]
}

func TestBootstrapOnce(t *testing.T) {
	s, _ := newService(t)
	pw := bootstrap(t, s)
	if len(pw) != GeneratedPasswordLength {
		t.Fatalf("password length %d", len(pw))
	}
	var out bytes.Buffer
	created, err := s.Bootstrap(context.Background(), "admin", &out)
	if err != nil || created || out.Len() != 0 {
		t.Fatalf("second bootstrap must be silent: created=%v err=%v out=%q", created, err, out.String())
	}
	events, _ := s.Store.AuthEvents(context.Background(), 10)
	if len(events) != 1 || events[0].Event != EventAdminCreated {
		t.Fatalf("events: %+v", events)
	}
}

func TestBootstrapConcurrentProcesses(t *testing.T) {
	// Two "processes" (separate store handles) on the same file.
	path := filepath.Join(t.TempDir(), store.DBFileName)
	h, _ := NewHasher(testParams)
	ctx := context.Background()
	var wg sync.WaitGroup
	outs := make([]bytes.Buffer, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := store.OpenPath(ctx, path)
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = st.Close() }()
			s := &Service{Store: st, Hasher: h, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				IdleTimeout: time.Hour, MaxAge: time.Hour}
			_, errs[i] = s.Bootstrap(ctx, "admin", &outs[i])
		}()
	}
	wg.Wait()
	banners := 0
	for i := range 2 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		banners += strings.Count(outs[i].String(), "INITIAL ADMIN ACCOUNT CREATED")
	}
	if banners != 1 {
		t.Fatalf("banner printed %d times, want exactly 1", banners)
	}
}

func TestLoginFlowAndLockout(t *testing.T) {
	s, clk := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	meta := ClientMeta{IP: "1.2.3.4", UserAgent: "test"}

	if _, err := s.Login(ctx, "nobody", pw, meta); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	for i := range LockoutThreshold {
		if _, err := s.Login(ctx, "admin", "wrong-password", meta); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Locked for 1 minute: even the right password is refused.
	if _, err := s.Login(ctx, "admin", pw, meta); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("locked account accepted the right password: %v", err)
	}
	clk.Add(61 * time.Second)
	// One more failure after the lock: next step is 5 minutes.
	if _, err := s.Login(ctx, "admin", "wrong-password", meta); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	u, _ := s.Store.UserByUsername(ctx, "admin")
	if u.LockedUntil == nil || *u.LockedUntil != clk.Now().Add(5*time.Minute).Unix() {
		t.Fatalf("expected a 5 minute lock, got %v", u.LockedUntil)
	}
	clk.Add(5*time.Minute + time.Second)
	ns, err := s.Login(ctx, "ADMIN", pw, meta)
	if err != nil {
		t.Fatalf("login after lock: %v", err)
	}
	if !ns.Session.MustChangePassword {
		t.Fatal("first session must require a password change")
	}
	u, _ = s.Store.UserByUsername(ctx, "admin")
	if u.FailedAttempts != 0 || u.LockedUntil != nil {
		t.Fatalf("counters not reset: %+v", u)
	}
	events, _ := s.Store.AuthEvents(ctx, 50)
	var lockouts int
	for _, e := range events {
		if e.Event == EventLockout {
			lockouts++
		}
	}
	if lockouts != 2 {
		t.Fatalf("expected 2 lockout events, got %d", lockouts)
	}
}

func TestLoginRehashes(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	stronger := testParams
	stronger.Time = 2
	s.Hasher, _ = NewHasher(stronger)
	if _, err := s.Login(ctx, "admin", pw, ClientMeta{}); err != nil {
		t.Fatal(err)
	}
	u, _ := s.Store.UserByUsername(ctx, "admin")
	if !strings.Contains(u.PasswordHash, "t=2,") {
		t.Fatalf("hash not upgraded: %s", u.PasswordHash)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, clk := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	ns, err := s.Login(ctx, "admin", pw, ClientMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, ns.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, ns.Token+"x"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("tampered token accepted")
	}
	// Activity every 11 h keeps the session alive until the absolute limit.
	for range 15 {
		clk.Add(11 * time.Hour)
		if _, err := s.Authenticate(ctx, ns.Token); err != nil {
			t.Fatalf("session expired too early at %s", clk.Now())
		}
	}
	clk.Add(11 * time.Hour) // 176 h > 168 h
	if _, err := s.Authenticate(ctx, ns.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("absolute max age not enforced")
	}

	ns, _ = s.Login(ctx, "admin", pw, ClientMeta{})
	clk.Add(12 * time.Hour)
	if _, err := s.Authenticate(ctx, ns.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("idle timeout not enforced")
	}
}

func TestTouchIsThrottled(t *testing.T) {
	s, clk := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	ns, _ := s.Login(ctx, "admin", pw, ClientMeta{})
	start := clk.Now().Unix()
	clk.Add(30 * time.Second)
	sess, _ := s.Authenticate(ctx, ns.Token)
	if sess.LastSeenAt != start {
		t.Fatal("last_seen_at updated before one minute")
	}
	clk.Add(31 * time.Second)
	sess, _ = s.Authenticate(ctx, ns.Token)
	if sess.LastSeenAt != clk.Now().Unix() {
		t.Fatal("last_seen_at not updated after one minute")
	}
}

func TestChangePasswordRotatesSessions(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	a, _ := s.Login(ctx, "admin", pw, ClientMeta{})
	b, _ := s.Login(ctx, "admin", pw, ClientMeta{})

	if _, _, err := s.ChangePassword(ctx, a.Session, "bad", "N3w-Strong-Pass!", "N3w-Strong-Pass!", ClientMeta{}); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong current password: %v", err)
	}
	_, v, err := s.ChangePassword(ctx, a.Session, pw, pw, pw, ClientMeta{})
	if err != nil || !slices.Contains(v, ViolationSameAsCurrent) {
		t.Fatalf("same password: v=%v err=%v", v, err)
	}
	_, v, _ = s.ChangePassword(ctx, a.Session, pw, "N3w-Strong-Pass!", "N3w-Strong-Pass?", ClientMeta{})
	if !slices.Equal(v, []Violation{ViolationMismatch}) {
		t.Fatalf("mismatch: %v", v)
	}
	_, v, _ = s.ChangePassword(ctx, a.Session, pw, "password", "password", ClientMeta{})
	if !slices.Contains(v, ViolationCommon) {
		t.Fatalf("common password not rejected: %v", v)
	}

	ns, v, err := s.ChangePassword(ctx, a.Session, pw, "N3w-Strong-Pass!", "N3w-Strong-Pass!", ClientMeta{})
	if err != nil || v != nil {
		t.Fatalf("change: v=%v err=%v", v, err)
	}
	if ns.Session.MustChangePassword {
		t.Fatal("new session must not require a change")
	}
	for _, old := range []string{a.Token, b.Token} {
		if _, err := s.Authenticate(ctx, old); !errors.Is(err, ErrSessionInvalid) {
			t.Fatal("old sessions must be revoked")
		}
	}
	sess, err := s.Authenticate(ctx, ns.Token)
	if err != nil || sess.MustChangePassword {
		t.Fatalf("rotated session: %+v %v", sess, err)
	}
	if _, err := s.Login(ctx, "admin", pw, ClientMeta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password still accepted")
	}
}

func TestResetPassword(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	ns, _ := s.Login(ctx, "admin", pw, ClientMeta{})
	_, _, _ = s.ChangePassword(ctx, ns.Session, pw, "N3w-Strong-Pass!", "N3w-Strong-Pass!", ClientMeta{})
	for range 6 {
		_, _ = s.Login(ctx, "admin", "wrong", ClientMeta{})
	}

	var out bytes.Buffer
	if err := s.ResetPassword(ctx, "admin", &out); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`: (\S{24})\n`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no password in output: %q", out.String())
	}
	u, _ := s.Store.UserByUsername(ctx, "admin")
	if !u.MustChangePassword || u.FailedAttempts != 0 || u.LockedUntil != nil {
		t.Fatalf("reset did not unlock / flag: %+v", u)
	}
	got, err := s.Login(ctx, "admin", m[1], ClientMeta{})
	if err != nil || !got.Session.MustChangePassword {
		t.Fatalf("login with reset password: %v", err)
	}
	if err := s.ResetPassword(ctx, "ghost", io.Discard); err == nil {
		t.Fatal("unknown user must fail")
	}
}

func TestRevokeSession(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	pw := bootstrap(t, s)
	a, _ := s.Login(ctx, "admin", pw, ClientMeta{IP: "1.1.1.1"})
	b, _ := s.Login(ctx, "admin", pw, ClientMeta{IP: "2.2.2.2"})
	list, _ := s.Sessions(ctx, a.Session.UserID)
	if len(list) != 2 {
		t.Fatalf("sessions: %d", len(list))
	}
	ok, err := s.RevokeSession(ctx, a.Session, b.Session.ID, ClientMeta{})
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, b.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("revoked session still valid")
	}
	if ok, _ := s.RevokeSession(ctx, a.Session, 9999, ClientMeta{}); ok {
		t.Fatal("revoking an unknown session must report false")
	}
}
