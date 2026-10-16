package httpx

import (
	"errors"
	"fmt"
)

// MaxPages bounds a pagination loop (at 50 to 1000 items a page, far beyond
// any library).
const MaxPages = 10_000

// ErrPagingIgnored: the server sent the same page twice, so it ignores the
// offset and the loop would never end.
var ErrPagingIgnored = errors.New("the server ignores the page offset (same page twice)")

// PageGuard stops a pagination loop that makes no progress: a page that
// starts with the same item as the previous one, or too many pages. Proxies
// and some server versions ignore the offset and send the first page again
// and again, without a total.
type PageGuard struct {
	pages int
	first string
}

// Next checks the page about to be appended, given the key of its first
// item ("" for an empty page).
func (g *PageGuard) Next(firstKey string) error {
	g.pages++
	if g.pages > MaxPages {
		return fmt.Errorf("pagination stopped after %d pages", MaxPages)
	}
	if g.pages > 1 && firstKey != "" && firstKey == g.first {
		return ErrPagingIgnored
	}
	g.first = firstKey
	return nil
}
