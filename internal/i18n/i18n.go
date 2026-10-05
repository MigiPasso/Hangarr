// Package i18n translates the user interface (English, the default, and
// French) and formats numbers, sizes and dates for the chosen language.
//
// Messages are keyed by their English text, gettext style: T("%d items")
// looks the key up in the French catalog and falls back to English. Keys may
// contain fmt verbs; arguments are applied after translation. A "|context"
// suffix tells apart identical English texts ("%d series|many"); it is never
// shown.
package i18n

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Lang is a supported language.
type Lang string

// Supported languages.
const (
	EN Lang = "en"
	FR Lang = "fr"
)

// Langs lists the supported languages in display order.
var Langs = []Lang{EN, FR}

// Parse returns the language for a code ("en", "fr"); ok is false otherwise.
func Parse(code string) (Lang, bool) {
	switch Lang(strings.ToLower(strings.TrimSpace(code))) {
	case EN:
		return EN, true
	case FR:
		return FR, true
	}
	return "", false
}

// catalogs maps a language to its translations of the English keys.
var catalogs = map[Lang]map[string]string{FR: fr}

// Localizer translates and formats for one language.
type Localizer struct{ Lang Lang }

// For returns the localizer of lang.
func For(lang Lang) Localizer { return Localizer{Lang: lang} }

// T translates key and applies args (fmt verbs).
func (l Localizer) T(key string, args ...any) string {
	msg, _, _ := strings.Cut(key, "|")
	if c, ok := catalogs[l.Lang]; ok {
		if v, ok := c[key]; ok {
			msg = v
		}
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// N picks the singular or plural key for n and formats it with n first:
// N(3, "%d item", "%d items"). French uses the singular for 0 and 1.
func (l Localizer) N(n int64, one, many string, args ...any) string {
	key := many
	if n == 1 || (l.Lang == FR && n == 0) {
		key = one
	}
	return l.T(key, append([]any{n}, args...)...)
}

// Has reports whether key has a translation (tests).
func Has(lang Lang, key string) bool {
	if lang == EN {
		return true
	}
	_, ok := catalogs[lang][key]
	return ok
}

// Keys lists the translated keys of lang (tests).
func Keys(lang Lang) []string {
	out := make([]string, 0, len(catalogs[lang]))
	for k := range catalogs[lang] {
		out = append(out, k)
	}
	return out
}

func (l Localizer) decimalSep(s string) string {
	if l.Lang == FR {
		return strings.Replace(s, ".", ",", 1)
	}
	return s
}

// Num formats f with at most one decimal ("2.5", "2,5", "3").
func (l Localizer) Num(f float64) string {
	s := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', 1, 64), "0"), ".")
	return l.decimalSep(s)
}

// Fixed formats f with exactly prec decimals.
func (l Localizer) Fixed(f float64, prec int) string {
	return l.decimalSep(strconv.FormatFloat(f, 'f', prec, 64))
}

// Pct formats a 0..1 share as a percentage with one decimal: "16.7%", "16,7 %".
func (l Localizer) Pct(share float64) string {
	if l.Lang == FR {
		return l.Fixed(share*100, 1) + " %"
	}
	return l.Fixed(share*100, 1) + "%"
}

// PctShort formats a 0..1 share with at most one decimal: "35%", "35 %".
func (l Localizer) PctShort(share float64) string {
	if l.Lang == FR {
		return l.Num(share*100) + " %"
	}
	return l.Num(share*100) + "%"
}

// Bytes formats a size with decimal units: "12.5 GB", "12,5 Go".
func (l Localizer) Bytes(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB", "PB"}
	if l.Lang == FR {
		units = []string{"o", "Ko", "Mo", "Go", "To", "Po"}
	}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[0])
	}
	prec := 1
	if f >= 100 {
		prec = 0
	}
	return l.Fixed(f, prec) + " " + units[i]
}

// Date formats a day: "2026-10-05" (en) or "05/10/2026" (fr).
func (l Localizer) Date(t time.Time) string {
	if l.Lang == FR {
		return t.Format("02/01/2006")
	}
	return t.Format("2006-01-02")
}

// DateTime formats a day and time.
func (l Localizer) DateTime(t time.Time) string {
	return l.Date(t) + " " + t.Format("15:04")
}

// DayMonth formats a short day for chart axes: "10-05" / "05/10".
func (l Localizer) DayMonth(t time.Time) string {
	if l.Lang == FR {
		return t.Format("02/01")
	}
	return t.Format("01-02")
}

// Relative formats t relative to now: "2 h ago", "il y a 2 h".
func (l Localizer) Relative(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return l.T("just now")
	case d < time.Hour:
		return l.T("%d min ago", int64(d/time.Minute))
	case d < 24*time.Hour:
		return l.T("%d h ago", int64(d/time.Hour))
	case d < 30*24*time.Hour:
		return l.N(int64(d/(24*time.Hour)), "%d day ago", "%d days ago")
	case d < 365*24*time.Hour:
		return l.N(int64(d/(30*24*time.Hour)), "%d month ago", "%d months ago")
	default:
		return l.N(int64(d/(365*24*time.Hour)), "%d year ago", "%d years ago")
	}
}

// Duration formats seconds: "42 min", "1 h 05 min", "30 s".
func (l Localizer) Duration(secs int64) string {
	if secs < 60 {
		return fmt.Sprintf("%d s", secs)
	}
	h, m := secs/3600, secs%3600/60
	if h == 0 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d min", h, m)
}

// Hours formats seconds as hours and minutes: "2h20m", "36h", "45m".
func (l Localizer) Hours(secs int64) string {
	m := (secs + 30) / 60
	h, m := m/60, m%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}
