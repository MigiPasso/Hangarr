package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// HashParams are argon2id parameters. Memory is in KiB.
type HashParams struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultHashParams: argon2id with m=64 MiB, t=3, p=2, 16-byte salt, 32-byte key.
var DefaultHashParams = HashParams{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// MaxConcurrentHashes bounds memory use: each hash allocates Memory KiB.
const MaxConcurrentHashes = 2

var errBadHash = errors.New("invalid argon2id hash")

// Hasher hashes and verifies passwords with argon2id, limiting concurrency.
type Hasher struct {
	params HashParams
	sem    chan struct{}
	dummy  string
}

// NewHasher builds a Hasher; it precomputes a dummy hash used to keep the
// response time constant when the username does not exist.
func NewHasher(p HashParams) (*Hasher, error) {
	h := &Hasher{params: p, sem: make(chan struct{}, MaxConcurrentHashes)}
	pw, err := GeneratePassword(GeneratedPasswordLength, "")
	if err != nil {
		return nil, err
	}
	if h.dummy, err = h.Hash(context.Background(), pw); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC string for the NFKC-normalized password.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	p := h.params
	key := argon2.IDKey([]byte(NormalizePassword(password)), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func parsePHC(encoded string) (HashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return HashParams{}, nil, nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return HashParams{}, nil, nil, errBadHash
	}
	var p HashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return HashParams{}, nil, nil, errBadHash
	}
	if p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return HashParams{}, nil, nil, errBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return HashParams{}, nil, nil, errBadHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return HashParams{}, nil, nil, errBadHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key)) //nolint:gosec // lengths come from a short decoded string
	return p, salt, key, nil
}

// Verify checks password against encoded. needsRehash is true when the
// stored parameters differ from the Hasher's current ones.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	p, salt, key, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(NormalizePassword(password)), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// VerifyDummy burns the same CPU/memory as a real verification; its result
// is always false. Used for unknown users and locked accounts.
func (h *Hasher) VerifyDummy(ctx context.Context, password string) {
	_, _, _ = h.Verify(ctx, password, h.dummy)
}
