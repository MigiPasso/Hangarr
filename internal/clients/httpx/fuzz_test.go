package httpx

import (
	"encoding/json"
	"testing"
)

// The tolerant types accept odd upstream JSON without panicking.
func FuzzFlexTypes(f *testing.F) {
	for _, s := range []string{`1`, `"2"`, `3.5`, `"x"`, `null`, `true`, `"2026-10-01T00:00:00Z"`, `1790812800`, `{}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var i Int
		_ = json.Unmarshal(b, &i)
		var fl Float
		_ = json.Unmarshal(b, &fl)
		var s String
		_ = json.Unmarshal(b, &s)
		var tm Time
		_ = json.Unmarshal(b, &tm)
	})
}
