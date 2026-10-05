package httpx

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Upstream APIs are loose with types (Tautulli returns numbers as strings,
// empty strings for missing values, Plex keys as strings or numbers). These
// types accept every variant and fall back to the zero value.

// Int decodes a JSON number, numeric string, "", null or bool.
type Int int64

// UnmarshalJSON implements json.Unmarshaler.
func (v *Int) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*v = 0
	switch {
	case len(b) == 0, string(b) == "null", string(b) == `""`, string(b) == "false":
		return nil
	case string(b) == "true":
		*v = 1
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*v = Int(n)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*v = Int(int64(f))
	}
	return nil
}

// Float decodes a JSON number, numeric string, "" or null.
type Float float64

// UnmarshalJSON implements json.Unmarshaler.
func (v *Float) UnmarshalJSON(b []byte) error {
	*v = 0
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*v = Float(f)
	}
	return nil
}

// String decodes a JSON string or number (as its text), null as "".
type String string

// UnmarshalJSON implements json.Unmarshaler.
func (v *String) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*v = ""
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil //nolint:nilerr // tolerant by design
		}
		*v = String(s)
		return nil
	}
	*v = String(b)
	return nil
}

// Time decodes an RFC 3339 timestamp; invalid values and the .NET zero date
// ("0001-01-01T00:00:00Z") used by the *arr apps become the zero time.
type Time struct{ time.Time }

// UnmarshalJSON implements json.Unmarshaler.
func (v *Time) UnmarshalJSON(b []byte) error {
	v.Time = time.Time{}
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return nil //nolint:nilerr // tolerant by design
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return nil //nolint:nilerr // tolerant by design
	}
	v.Time = t
	return nil
}

// UnixPtr returns the Unix time or nil for the zero time.
func (v Time) UnixPtr() *int64 {
	if v.IsZero() {
		return nil
	}
	u := v.Unix()
	return &u
}
