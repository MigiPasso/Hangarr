// Package plex is a read client for the Plex Media Server API (JSON).
//
// Watch fields of library endpoints (viewCount, lastViewedAt,
// viewedLeafCount) only describe the token owner's account: they are
// deliberately not decoded. Watch statistics come from Tautulli, or from the
// all-accounts history endpoint as a fallback.
package plex

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"cleanarr/internal/clients/httpx"
)

// Library item types for /library/sections/{key}/all?type=N.
const (
	TypeMovie  = 1
	TypeShow   = 2
	TypeSeason = 3
)

// pageSize is the X-Plex-Container-Size used for paginated listings.
const pageSize = 500

// Section is a library section.
type Section struct {
	Key   httpx.String `json:"key"`
	Type  string       `json:"type"` // movie | show | artist | photo
	Title string       `json:"title"`
}

// GUID is an external identifier ("imdb://tt…", "tmdb://…", "tvdb://…").
type GUID struct {
	ID string `json:"id"`
}

// Item is a movie, show or season.
type Item struct {
	RatingKey        httpx.String `json:"ratingKey"`
	ParentRatingKey  httpx.String `json:"parentRatingKey"`
	Type             string       `json:"type"`
	Title            string       `json:"title"`
	ParentTitle      string       `json:"parentTitle"`
	Year             httpx.Int    `json:"year"`
	Index            httpx.Int    `json:"index"`
	LeafCount        httpx.Int    `json:"leafCount"`
	AddedAt          httpx.Int    `json:"addedAt"`
	Duration         httpx.Int    `json:"duration"`
	LibrarySectionID httpx.Int    `json:"librarySectionID"`
	// Legacy agents only fill guid ("com.plexapp.agents.imdb://tt…?lang=en");
	// the Plex agents fill the Guid array (requires includeGuids=1).
	LegacyGUID string `json:"guid"`
	GUIDs      []GUID `json:"Guid"`
}

var (
	legacyIMDb = regexp.MustCompile(`agents\.imdb://(tt\d+)`)
	legacyTMDb = regexp.MustCompile(`agents\.themoviedb://(\d+)`)
	legacyTVDb = regexp.MustCompile(`agents\.thetvdb://(\d+)`)
	imdbID     = regexp.MustCompile(`^tt\d+$`)
)

// ExternalIDs extracts TVDB, TMDB and IMDb ids from both GUID formats.
func (i Item) ExternalIDs() (tvdb, tmdb int64, imdb string) {
	for _, g := range i.GUIDs {
		scheme, value, ok := strings.Cut(g.ID, "://")
		if !ok {
			continue
		}
		switch scheme {
		case "tvdb":
			tvdb, _ = strconv.ParseInt(value, 10, 64)
		case "tmdb":
			tmdb, _ = strconv.ParseInt(value, 10, 64)
		case "imdb":
			if imdbID.MatchString(value) {
				imdb = value
			}
		}
	}
	if m := legacyIMDb.FindStringSubmatch(i.LegacyGUID); m != nil && imdb == "" {
		imdb = m[1]
	}
	if m := legacyTMDb.FindStringSubmatch(i.LegacyGUID); m != nil && tmdb == 0 {
		tmdb, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := legacyTVDb.FindStringSubmatch(i.LegacyGUID); m != nil && tvdb == 0 {
		tvdb, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return tvdb, tmdb, imdb
}

// Account is a Plex account known to the server.
type Account struct {
	ID   httpx.Int `json:"id"`
	Name string    `json:"name"`
}

// HistoryEntry is one row of /status/sessions/history/all. Plex only records
// a view once an item is considered watched.
type HistoryEntry struct {
	HistoryKey       string       `json:"historyKey"` // "/status/sessions/history/123"
	RatingKey        httpx.String `json:"ratingKey"`
	ParentKey        string       `json:"parentKey"`      // "/library/metadata/123"
	GrandparentKey   string       `json:"grandparentKey"` // "/library/metadata/123"
	Type             string       `json:"type"`           // movie | episode
	Title            string       `json:"title"`
	GrandparentTitle string       `json:"grandparentTitle"`
	Index            httpx.Int    `json:"index"`
	ParentIndex      httpx.Int    `json:"parentIndex"`
	ViewedAt         httpx.Int    `json:"viewedAt"`
	AccountID        httpx.Int    `json:"accountID"`
	LibrarySectionID httpx.String `json:"librarySectionID"`
	Year             httpx.Int    `json:"year"`
}

// ID returns the numeric history id.
func (h HistoryEntry) ID() int64 {
	n, _ := strconv.ParseInt(h.HistoryKey[strings.LastIndex(h.HistoryKey, "/")+1:], 10, 64)
	return n
}

// KeyID returns the trailing rating key of a "/library/metadata/123" path.
func KeyID(path string) string {
	if path == "" {
		return ""
	}
	return path[strings.LastIndex(path, "/")+1:]
}

type container[T any] struct {
	MediaContainer struct {
		Size      httpx.Int `json:"size"`
		TotalSize httpx.Int `json:"totalSize"`
		Metadata  []T       `json:"Metadata"`
		Directory []T       `json:"Directory"`
		Account   []T       `json:"Account"`
	} `json:"MediaContainer"`
}

// Client reads from a Plex server.
type Client struct{ HTTP *httpx.Client }

// New builds a client authenticated with the X-Plex-Token header.
func New(baseURL, token string) *Client {
	return &Client{HTTP: httpx.New("plex", baseURL, func(r *http.Request) { r.Header.Set("X-Plex-Token", token) })}
}

// Sections lists library sections.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var out container[Section]
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: "/library/sections"}, &out); err != nil {
		return nil, err
	}
	return out.MediaContainer.Directory, nil
}

// page fetches one page of a paginated listing; it returns the items and
// the total size reported by the server.
func page[T any](ctx context.Context, c *Client, path string, q url.Values, start int) ([]T, int, error) {
	q.Set("X-Plex-Container-Start", strconv.Itoa(start))
	q.Set("X-Plex-Container-Size", strconv.Itoa(pageSize))
	var out container[T]
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: path, Query: q}, &out); err != nil {
		return nil, 0, err
	}
	return out.MediaContainer.Metadata, int(out.MediaContainer.TotalSize), nil
}

// Items lists every item of a type in a section, following pagination.
func (c *Client) Items(ctx context.Context, sectionKey string, itemType int) ([]Item, error) {
	path := "/library/sections/" + url.PathEscape(sectionKey) + "/all"
	var all []Item
	for {
		q := url.Values{"type": {strconv.Itoa(itemType)}, "includeGuids": {"1"}}
		items, total, err := page[Item](ctx, c, path, q, len(all))
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize || (total > 0 && len(all) >= total) {
			return all, nil
		}
	}
}

// Accounts lists the server's accounts (id → name).
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var out container[Account]
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: "/accounts"}, &out); err != nil {
		return nil, err
	}
	return out.MediaContainer.Account, nil
}

// History returns one page of the all-accounts history, newest first.
func (c *Client) History(ctx context.Context, start int) ([]HistoryEntry, int, error) {
	return page[HistoryEntry](ctx, c, "/status/sessions/history/all", url.Values{"sort": {"viewedAt:desc"}}, start)
}

// HistoryPageSize is the page size used by History.
const HistoryPageSize = pageSize

// Identity returns the server's machine identifier (used for web links).
func (c *Client) Identity(ctx context.Context) (string, error) {
	id, err := c.identity(ctx)
	return id.MachineIdentifier, err
}

type identity struct {
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
}

func (c *Client) identity(ctx context.Context) (identity, error) {
	var out struct {
		MediaContainer identity `json:"MediaContainer"`
	}
	err := c.HTTP.JSON(ctx, httpx.Request{Path: "/identity"}, &out)
	return out.MediaContainer, err
}

// Check tests the URL and the token and returns the server version.
// /identity answers without a token on a real server, so the token is
// checked with the sections listing.
func (c *Client) Check(ctx context.Context) (string, error) {
	id, err := c.identity(ctx)
	if err != nil {
		return "", err
	}
	if _, err := c.Sections(ctx); err != nil {
		return "", err
	}
	return id.Version, nil
}

// RefreshSection asks Plex to scan a library section.
func (c *Client) RefreshSection(ctx context.Context, key string) error {
	_, err := c.HTTP.Do(ctx, httpx.Request{Path: "/library/sections/" + url.PathEscape(key) + "/refresh"})
	return err
}

// EmptyTrash removes items whose files disappeared from a section.
func (c *Client) EmptyTrash(ctx context.Context, key string) error {
	_, err := c.HTTP.Do(ctx, httpx.Request{Method: http.MethodPut, Path: "/library/sections/" + url.PathEscape(key) + "/emptyTrash"})
	return err
}
