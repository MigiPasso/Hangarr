// Package jellyfin is a client for the Jellyfin server API (JSON).
package jellyfin

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"hangarr/internal/clients/httpx"
)

// Item types, as used by includeItemTypes.
const (
	TypeMovie   = "Movie"
	TypeSeries  = "Series"
	TypeSeason  = "Season"
	TypeEpisode = "Episode"
)

// pageSize is the limit used by the paginated listings.
const pageSize = 500

// PlayedPageSize is the page size used by Played.
const PlayedPageSize = pageSize

// UserData is the state of an item for one user.
type UserData struct {
	Played                bool        `json:"Played"`
	PlayCount             httpx.Int   `json:"PlayCount"`
	LastPlayedDate        httpx.Time  `json:"LastPlayedDate"`
	PlaybackPositionTicks httpx.Int   `json:"PlaybackPositionTicks"`
	PlayedPercentage      httpx.Float `json:"PlayedPercentage"`
}

// Item is a movie, series, season or episode. Ticks are 100 ns units.
type Item struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	OriginalTitle     string            `json:"OriginalTitle"`
	Type              string            `json:"Type"`
	ProductionYear    httpx.Int         `json:"ProductionYear"`
	ProviderIDs       map[string]string `json:"ProviderIds"`
	Path              string            `json:"Path"`
	DateCreated       httpx.Time        `json:"DateCreated"`
	RunTimeTicks      httpx.Int         `json:"RunTimeTicks"`
	SeriesID          string            `json:"SeriesId"`
	SeriesName        string            `json:"SeriesName"`
	IndexNumber       httpx.Int         `json:"IndexNumber"`
	ParentIndexNumber httpx.Int         `json:"ParentIndexNumber"`
	UserData          *UserData         `json:"UserData"`
	MediaSourceCount  httpx.Int         `json:"MediaSourceCount"`
	MediaSources      []MediaSource     `json:"MediaSources"`
}

// MediaSource is one version of a video.
type MediaSource struct {
	ID           string        `json:"Id"` // item id of that version
	Path         string        `json:"Path"`
	Name         string        `json:"Name"`
	Container    string        `json:"Container"`
	Size         httpx.Int     `json:"Size"`
	Bitrate      httpx.Int     `json:"Bitrate"`
	MediaStreams []MediaStream `json:"MediaStreams"`
}

// MediaStream is a video, audio or subtitle track.
type MediaStream struct {
	Type         string    `json:"Type"` // "Video", "Audio", "Subtitle"
	Codec        string    `json:"Codec"`
	Language     string    `json:"Language"` // ISO 639-2 ("fra"), may be empty
	DisplayTitle string    `json:"DisplayTitle"`
	Height       httpx.Int `json:"Height"`
	Width        httpx.Int `json:"Width"`
	IsForced     bool      `json:"IsForced"`
	IsExternal   bool      `json:"IsExternal"`
}

var (
	imdbID    = regexp.MustCompile(`^tt\d+$`)
	idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ExternalIDs returns the TVDB, TMDB and IMDb ids. Provider keys match in any
// case; invalid values are ignored.
func (i Item) ExternalIDs() (tvdb, tmdb int64, imdb string) {
	for provider, value := range i.ProviderIDs {
		switch strings.ToLower(provider) {
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
	return tvdb, tmdb, imdb
}

// Policy holds the access flags of a user.
type Policy struct {
	IsAdministrator bool `json:"IsAdministrator"`
	IsDisabled      bool `json:"IsDisabled"`
	IsHidden        bool `json:"IsHidden"`
}

// User is a Jellyfin account.
type User struct {
	ID     string  `json:"Id"`
	Name   string  `json:"Name"`
	Policy *Policy `json:"Policy"`
}

// SystemInfo identifies the server.
type SystemInfo struct {
	ServerName  string `json:"ServerName"`
	Version     string `json:"Version"`
	ProductName string `json:"ProductName"`
	ID          string `json:"Id"`
}

// Client reads from a Jellyfin server and deletes items on it.
type Client struct{ HTTP *httpx.Client }

// New builds a client authenticated with the MediaBrowser token header. The
// X-Emby-Token header and the api_key parameter are disabled by default on
// Jellyfin 12.
func New(baseURL, apiKey string) *Client {
	return &Client{HTTP: httpx.New("jellyfin", baseURL, func(r *http.Request) {
		r.Header.Set("Authorization", `MediaBrowser Token="`+apiKey+`"`)
	})}
}

// SystemInfo returns the server identity. The endpoint requires
// authentication, so it also checks the API key.
func (c *Client) SystemInfo(ctx context.Context) (SystemInfo, error) {
	var out SystemInfo
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: "/System/Info"}, &out); err != nil {
		return SystemInfo{}, err
	}
	return out, nil
}

// Users lists the accounts of the server.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out []User
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: "/Users"}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// queryResult is the envelope of GET /Items.
type queryResult[T any] struct {
	Items            []T       `json:"Items"`
	TotalRecordCount httpx.Int `json:"TotalRecordCount"`
	StartIndex       httpx.Int `json:"StartIndex"`
}

// query fetches one page of GET /Items; it returns the items and the total
// record count reported by the server.
func query[T any](ctx context.Context, c *Client, q url.Values, start int) ([]T, int, error) {
	q.Set("startIndex", strconv.Itoa(start))
	q.Set("limit", strconv.Itoa(pageSize))
	var out queryResult[T]
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: "/Items", Query: q}, &out); err != nil {
		return nil, 0, err
	}
	return out.Items, int(out.TotalRecordCount), nil
}

// collect follows the pages of a GET /Items listing and returns every item.
func collect(ctx context.Context, c *Client, q url.Values) ([]Item, error) {
	var all []Item
	var guard httpx.PageGuard
	for {
		items, total, err := query[Item](ctx, c, q, len(all))
		if err != nil {
			return nil, err
		}
		first := ""
		if len(items) > 0 {
			first = items[0].ID
		}
		if err := guard.Next(first); err != nil {
			return nil, fmt.Errorf("jellyfin: %w", err)
		}
		all = append(all, items...)
		if len(items) < pageSize || (total > 0 && len(all) >= total) {
			return all, nil
		}
	}
}

// Items lists every item of a type, following pagination.
func (c *Client) Items(ctx context.Context, itemType string) ([]Item, error) {
	return collect(ctx, c, url.Values{
		"recursive":        {"true"},
		"includeItemTypes": {itemType},
		"fields":           {"ProviderIds,Path,DateCreated,OriginalTitle,MediaSourceCount"},
		"sortBy":           {"SortName"},
		"enableImages":     {"false"},
	})
}

// Played returns one page of the movies and episodes a user has played, most
// recent first, with the size of the whole list.
func (c *Client) Played(ctx context.Context, userID string, start int) ([]Item, int, error) {
	return query[Item](ctx, c, url.Values{
		"userId":           {userID},
		"recursive":        {"true"},
		"includeItemTypes": {TypeMovie + "," + TypeEpisode},
		"isPlayed":         {"true"},
		"sortBy":           {"DatePlayed"},
		"sortOrder":        {"Descending"},
		"enableUserData":   {"true"},
		"enableImages":     {"false"},
	}, start)
}

// Resumable lists every movie and episode a user has started but not finished.
func (c *Client) Resumable(ctx context.Context, userID string) ([]Item, error) {
	return collect(ctx, c, url.Values{
		"userId":           {userID},
		"recursive":        {"true"},
		"includeItemTypes": {TypeMovie + "," + TypeEpisode},
		"filters":          {"IsResumable"},
		"enableUserData":   {"true"},
		"enableImages":     {"false"},
	})
}

// versionsBatch is the number of ids sent per Versions request.
const versionsBatch = 50

// Versions returns the items with their versions (one media source per file).
// The ids are sent in batches; every id must have the format of IsID.
func (c *Client) Versions(ctx context.Context, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	for _, id := range ids {
		if !IsID(id) {
			return nil, fmt.Errorf("jellyfin: invalid item id %q", id)
		}
	}
	var all []Item
	for start := 0; start < len(ids); start += versionsBatch {
		batch := ids[start:min(start+versionsBatch, len(ids))]
		items, _, err := query[Item](ctx, c, url.Values{
			"ids":          {strings.Join(batch, ",")},
			"fields":       {"Path,MediaSources,MediaSourceCount,ProviderIds"},
			"enableImages": {"false"},
		}, 0)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	return all, nil
}

// DeleteItem deletes an item and its files from disk.
func (c *Client) DeleteItem(ctx context.Context, id string) error {
	_, err := c.HTTP.Do(ctx, httpx.Request{Method: http.MethodDelete, Path: "/Items/" + url.PathEscape(id)})
	return err
}

// RefreshLibrary asks Jellyfin to scan its libraries.
func (c *Client) RefreshLibrary(ctx context.Context) error {
	_, err := c.HTTP.Do(ctx, httpx.Request{Method: http.MethodPost, Path: "/Library/Refresh"})
	return err
}

// IsID reports whether s has the format of a Jellyfin id: 32 lowercase hex
// digits, without dashes.
func IsID(s string) bool {
	return idPattern.MatchString(s)
}

// AccountID returns a stable integer id for a Jellyfin user. It is above
// 1<<62, so it never collides with a Plex account id.
func AccountID(userID string) int64 {
	return 1<<62 | fnv62(strings.ToLower(userID))
}

// EventID returns a stable integer id for one user playing one item.
func EventID(userID, itemID string) int64 {
	return 1<<62 | fnv62(strings.ToLower(userID)+"|"+strings.ToLower(itemID))
}

// fnv62 is the 62-bit FNV-1a hash of s.
func fnv62(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() & (1<<62 - 1))
}
