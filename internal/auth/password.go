package auth

import (
	"bufio"
	"crypto/rand"
	_ "embed"
	"errors"
	"math/big"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Password length bounds, counted in Unicode code points after NFKC.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 128
)

// Violation identifies a password policy rule that was not met.
type Violation string

// Policy violations. Messages for the UI live in the web package.
const (
	ViolationTooShort         Violation = "too_short"
	ViolationTooLong          Violation = "too_long"
	ViolationNoLower          Violation = "no_lower"
	ViolationNoUpper          Violation = "no_upper"
	ViolationNoDigit          Violation = "no_digit"
	ViolationNoSpecial        Violation = "no_special"
	ViolationContainsUsername Violation = "contains_username"
	ViolationCommon           Violation = "common"
	ViolationSameAsCurrent    Violation = "same_as_current"
	ViolationMismatch         Violation = "mismatch"
)

// NormalizePassword applies NFKC; it is used before validation and hashing so
// that visually identical inputs produce the same hash.
func NormalizePassword(pw string) string {
	return norm.NFKC.String(pw)
}

//go:embed common-passwords.txt
var commonPasswordsRaw string

var commonPasswords = sync.OnceValue(func() map[string]struct{} {
	m := make(map[string]struct{}, 10_000)
	sc := bufio.NewScanner(strings.NewReader(commonPasswordsRaw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m[strings.ToLower(NormalizePassword(line))] = struct{}{}
	}
	return m
})

// IsCommonPassword reports whether pw is in the embedded list (case-insensitive).
func IsCommonPassword(pw string) bool {
	_, ok := commonPasswords()[strings.ToLower(NormalizePassword(pw))]
	return ok
}

// CheckPolicy validates a new password against the password policy, except the
// "different from current" rule which needs the stored hash.
func CheckPolicy(password, username string) []Violation {
	pw := NormalizePassword(password)
	var v []Violation
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLength {
		v = append(v, ViolationTooShort)
	}
	if n > MaxPasswordLength {
		v = append(v, ViolationTooLong)
	}
	var lower, upper, digit, special bool
	for _, r := range pw {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		case !unicode.IsLetter(r) && !unicode.IsNumber(r):
			special = true
		}
	}
	if !lower {
		v = append(v, ViolationNoLower)
	}
	if !upper {
		v = append(v, ViolationNoUpper)
	}
	if !digit {
		v = append(v, ViolationNoDigit)
	}
	if !special {
		v = append(v, ViolationNoSpecial)
	}
	if u := strings.ToLower(NormalizePassword(strings.TrimSpace(username))); u != "" &&
		strings.Contains(strings.ToLower(pw), u) {
		v = append(v, ViolationContainsUsername)
	}
	if IsCommonPassword(pw) {
		v = append(v, ViolationCommon)
	}
	return v
}

// Alphabets for generated passwords: no ambiguous characters (0/O, 1/l/I),
// no quotes, backslash or "$" so the value survives copy/paste into a shell.
const (
	genLower   = "abcdefghijkmnopqrstuvwxyz"
	genUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	genDigits  = "23456789"
	genSpecial = "!#%&*+-=?@^_~"
)

// GeneratedPasswordLength is the length of initial/reset passwords.
const GeneratedPasswordLength = 24

func randIndex(n int) (int, error) {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(i.Int64()), nil
}

// GeneratePassword returns a random password containing at least one
// character of each class and satisfying the policy for username.
func GeneratePassword(length int, username string) (string, error) {
	if length < 4 {
		return "", errors.New("password length must be >= 4")
	}
	classes := []string{genLower, genUpper, genDigits, genSpecial}
	all := genLower + genUpper + genDigits + genSpecial
	for {
		buf := make([]byte, length)
		for i := range buf {
			set := all
			if i < len(classes) {
				set = classes[i]
			}
			j, err := randIndex(len(set))
			if err != nil {
				return "", err
			}
			buf[i] = set[j]
		}
		// Fisher-Yates so the mandatory characters are not always first.
		for i := len(buf) - 1; i > 0; i-- {
			j, err := randIndex(i + 1)
			if err != nil {
				return "", err
			}
			buf[i], buf[j] = buf[j], buf[i]
		}
		pw := string(buf)
		if len(CheckPolicy(pw, username)) == 0 {
			return pw, nil
		}
	}
}
