// Package radarr is a read client for the Radarr v3 API.
package radarr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"arrstats/internal/clients/httpx"
	"arrstats/internal/clients/sonarr"
)

// Ratings accepts both shapes seen in Radarr: the current per-source map
// ({"imdb":{"value":8.1},"tmdb":{...}}) and the legacy flat {"value":8.1}.
type Ratings struct {
	IMDb  float64
	TMDb  float64
	Value float64
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *Ratings) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil //nolint:nilerr // tolerant by design
	}
	value := func(m json.RawMessage) float64 {
		var v struct {
			Value httpx.Float `json:"value"`
		}
		_ = json.Unmarshal(m, &v)
		return float64(v.Value)
	}
	if m, ok := raw["imdb"]; ok {
		r.IMDb = value(m)
	}
	if m, ok := raw["tmdb"]; ok {
		r.TMDb = value(m)
	}
	if m, ok := raw["value"]; ok {
		var f httpx.Float
		_ = json.Unmarshal(m, &f)
		r.Value = float64(f)
	}
	return nil
}

// Best returns the IMDb rating, then TMDb, then the legacy value.
func (r Ratings) Best() float64 {
	for _, v := range []float64{r.IMDb, r.TMDb, r.Value} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// Movie as returned by GET /api/v3/movie.
type Movie struct {
	ID               int64          `json:"id"`
	Title            string         `json:"title"`
	SortTitle        string         `json:"sortTitle"`
	TitleSlug        string         `json:"titleSlug"`
	Year             httpx.Int      `json:"year"`
	TmdbID           httpx.Int      `json:"tmdbId"`
	ImdbID           string         `json:"imdbId"`
	HasFile          bool           `json:"hasFile"`
	SizeOnDisk       httpx.Int      `json:"sizeOnDisk"`
	Genres           []string       `json:"genres"`
	Ratings          Ratings        `json:"ratings"`
	Overview         string         `json:"overview"`
	Images           []sonarr.Image `json:"images"`
	Added            httpx.Time     `json:"added"`
	Tags             []int64        `json:"tags"`
	QualityProfileID int64          `json:"qualityProfileId"`
	Studio           string         `json:"studio"`
	Runtime          httpx.Int      `json:"runtime"`
	Path             string         `json:"path"`
	Monitored        bool           `json:"monitored"`
	Status           string         `json:"status"`
	MovieFile        *struct {
		Size httpx.Int `json:"size"`
	} `json:"movieFile"`
}

// Client reads from Radarr.
type Client struct{ HTTP *httpx.Client }

// SystemStatus checks the URL and the API key and returns the version.
func (c *Client) SystemStatus(ctx context.Context) (*sonarr.SystemStatus, error) {
	var st sonarr.SystemStatus
	return &st, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/system/status"}, &st)
}

// New builds a client authenticated with the X-Api-Key header.
func New(baseURL, apiKey string) *Client {
	return &Client{HTTP: httpx.New("radarr", baseURL, func(r *http.Request) { r.Header.Set("X-Api-Key", apiKey) })}
}

// Movies lists every movie.
func (c *Client) Movies(ctx context.Context) ([]Movie, error) {
	var out []Movie
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/movie"}, &out)
}

// Tags lists tags.
func (c *Client) Tags(ctx context.Context) ([]sonarr.Tag, error) {
	var out []sonarr.Tag
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/tag"}, &out)
}

// QualityProfiles lists quality profiles.
func (c *Client) QualityProfiles(ctx context.Context) ([]sonarr.QualityProfile, error) {
	var out []sonarr.QualityProfile
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/qualityprofile"}, &out)
}

// MovieByID returns one movie.
func (c *Client) MovieByID(ctx context.Context, id int64) (*Movie, error) {
	m, _, err := c.MovieByIDRaw(ctx, id)
	return m, err
}

// MovieByIDRaw returns one movie, decoded, and its raw JSON (kept intact for
// PUT so that fields unknown to this client are preserved).
func (c *Client) MovieByIDRaw(ctx context.Context, id int64) (*Movie, json.RawMessage, error) {
	b, err := c.HTTP.Do(ctx, httpx.Request{Path: "/api/v3/movie/" + strconv.FormatInt(id, 10)})
	if err != nil {
		return nil, nil, err
	}
	var m Movie
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, nil, fmt.Errorf("radarr: decode movie %d: %w", id, err)
	}
	return &m, b, nil
}

// UpdateMovieRaw sends a full movie object (PUT /api/v3/movie/{id}).
func (c *Client) UpdateMovieRaw(ctx context.Context, id int64, raw json.RawMessage) error {
	return c.HTTP.JSON(ctx, httpx.Request{Method: http.MethodPut, Path: "/api/v3/movie/" + strconv.FormatInt(id, 10), Body: raw}, nil)
}

// CreateTag creates a tag and returns it with its id.
func (c *Client) CreateTag(ctx context.Context, label string) (*sonarr.Tag, error) {
	return sonarr.CreateTag(ctx, c.HTTP, label)
}

// DeleteMovie deletes a movie (and its files when deleteFiles).
func (c *Client) DeleteMovie(ctx context.Context, id int64, deleteFiles, importExclusion bool) error {
	q := url.Values{"deleteFiles": {strconv.FormatBool(deleteFiles)},
		"addImportExclusion": {strconv.FormatBool(importExclusion)}}
	return c.HTTP.JSON(ctx, httpx.Request{Method: http.MethodDelete, Path: "/api/v3/movie/" + strconv.FormatInt(id, 10), Query: q}, nil)
}

// DiskSpace lists the disks seen by Radarr.
func (c *Client) DiskSpace(ctx context.Context) ([]sonarr.DiskSpace, error) {
	var out []sonarr.DiskSpace
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/diskspace"}, &out)
}
