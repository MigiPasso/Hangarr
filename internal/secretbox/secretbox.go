// Package secretbox encrypts the service credentials stored in the database
// with AES-256-GCM. The master key lives outside the data volume
// (CLEANARR_MASTER_KEY or CLEANARR_MASTER_KEY_FILE): a copy of the database
// alone does not reveal the credentials.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the master key length (AES-256).
const KeySize = 32

// prefix versions the sealed format: "v1:" + base64(nonce || ciphertext).
const prefix = "v1:"

// ErrUndecryptable is returned when a value cannot be opened: wrong master
// key, tampered value or value bound to another name.
var ErrUndecryptable = errors.New("secret cannot be decrypted (master key changed?)")

// ParseKey decodes a master key given as 64 hex characters or as base64
// (standard or URL alphabet, padded or not) of 32 bytes.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 2*KeySize {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != KeySize {
				return nil, errors.New("master key must decode to 32 bytes")
			}
			return b, nil
		}
	}
	return nil, errors.New("master key must be 32 bytes, as 64 hex characters or base64")
}

// NewKey returns a fresh random key encoded in base64.
func NewKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Box seals and opens values.
type Box struct{ aead cipher.AEAD }

// New returns a box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext with a random nonce. name is authenticated
// (additional data): a value copied under another name does not open.
func (b *Box) Seal(name, plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(name))
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a value produced by Seal under the same name.
func (b *Box) Open(name, sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return "", ErrUndecryptable
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	ns := b.aead.NonceSize()
	if err != nil || len(data) < ns+b.aead.Overhead() {
		return "", ErrUndecryptable
	}
	plain, err := b.aead.Open(nil, data[:ns], data[ns:], []byte(name))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(plain), nil
}

// LoadOrCreateKeyFile reads the key stored at path, or creates it (mode
// 0600) with a fresh random key. The file is written aside then hard-linked
// into place: a concurrent process never reads a partial file, and both
// agree on the first key linked.
func LoadOrCreateKeyFile(path string) (key []byte, created bool, err error) {
	if key, err := ReadKeyFile(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, false, err
	}
	enc, err := NewKey()
	if err != nil {
		return nil, false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".master.key-*") // mode 0600
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.WriteString(enc + "\n")
	serr := tmp.Sync()
	if err := errors.Join(werr, serr, tmp.Close()); err != nil {
		return nil, false, err
	}
	if err := os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		key, err := ReadKeyFile(path)
		return key, false, err
	} else if err != nil {
		return nil, false, err
	}
	key, err = ParseKey(enc)
	return key, true, err
}

// ReadKeyFile reads a key file written by LoadOrCreateKeyFile.
func ReadKeyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is DATA_DIR/master.key
	if err != nil {
		return nil, err
	}
	key, err := ParseKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}
