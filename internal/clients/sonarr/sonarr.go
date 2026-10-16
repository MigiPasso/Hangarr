// Package sonarr is a read client for the Sonarr v3 API. Structs are
// tolerant: unknown fields are ignored and missing ones keep zero values.
package sonarr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"hangarr/internal/clients/httpx"
)

// Image is a poster/fanart/banner reference.
type Image struct {
	CoverType string `json:"coverType"`
	URL       string `json:"url"`
	RemoteURL string `json:"remoteUrl"`
}

// SeasonStatistics are per-season file statistics.
type SeasonStatistics struct {
	EpisodeFileCount  httpx.Int `json:"episodeFileCount"`
	EpisodeCount      httpx.Int `json:"episodeCount"`
	TotalEpisodeCount httpx.Int `json:"totalEpisodeCount"`
	SizeOnDisk        httpx.Int `json:"sizeOnDisk"`
}

// Season of a series.
type Season struct {
	SeasonNumber int               `json:"seasonNumber"`
	Monitored    bool              `json:"monitored"`
	Statistics   *SeasonStatistics `json:"statistics"`
}

// Series as returned by GET /api/v3/series.
type Series struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	SortTitle string    `json:"sortTitle"`
	TitleSlug string    `json:"titleSlug"`
	Year      httpx.Int `json:"year"`
	TvdbID    httpx.Int `json:"tvdbId"`
	TmdbID    httpx.Int `json:"tmdbId"`
	ImdbID    string    `json:"imdbId"`
	Status    string    `json:"status"`
	Network   string    `json:"network"`
	Genres    []string  `json:"genres"`
	Ratings   struct {
		Value httpx.Float `json:"value"`
	} `json:"ratings"`
	Overview         string            `json:"overview"`
	Images           []Image           `json:"images"`
	Seasons          []Season          `json:"seasons"`
	Statistics       *SeasonStatistics `json:"statistics"`
	Added            httpx.Time        `json:"added"`
	Tags             []int64           `json:"tags"`
	QualityProfileID int64             `json:"qualityProfileId"`
	Path             string            `json:"path"`
	Monitored        bool              `json:"monitored"`
	Runtime          httpx.Int         `json:"runtime"`
	AlternateTitles  []AlternateTitle  `json:"alternateTitles"`
}

// AlternateTitle is another title of a series or movie (other languages).
type AlternateTitle struct {
	Title string `json:"title"`
}

// Tag is a Sonarr/Radarr tag.
type Tag struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// QualityProfile is a quality profile (only the name is used).
type QualityProfile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Client reads from Sonarr.
type Client struct{ HTTP *httpx.Client }

// SystemStatus is the subset of GET /api/v3/system/status used to test a connection.
type SystemStatus struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// SystemStatus checks the URL and the API key and returns the version.
func (c *Client) SystemStatus(ctx context.Context) (*SystemStatus, error) {
	var st SystemStatus
	return &st, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/system/status"}, &st)
}

// New builds a client authenticated with the X-Api-Key header.
func New(baseURL, apiKey string) *Client {
	return &Client{HTTP: httpx.New("sonarr", baseURL, func(r *http.Request) { r.Header.Set("X-Api-Key", apiKey) })}
}

// Series lists every series.
func (c *Client) Series(ctx context.Context) ([]Series, error) {
	var out []Series
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/series"}, &out)
}

// Tags lists tags.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var out []Tag
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/tag"}, &out)
}

// QualityProfiles lists quality profiles.
func (c *Client) QualityProfiles(ctx context.Context) ([]QualityProfile, error) {
	var out []QualityProfile
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/qualityprofile"}, &out)
}

// Episode is an entry of GET /api/v3/episode?seriesId=.
type Episode struct {
	ID            int64      `json:"id"`
	SeriesID      int64      `json:"seriesId"`
	SeasonNumber  int        `json:"seasonNumber"`
	EpisodeNumber int        `json:"episodeNumber"`
	Title         string     `json:"title"`
	AirDateUTC    httpx.Time `json:"airDateUtc"`
	HasFile       bool       `json:"hasFile"`
	Monitored     bool       `json:"monitored"`
	EpisodeFileID int64      `json:"episodeFileId"`
}

// EpisodeFile is an entry of GET /api/v3/episodefile?seriesId=.
type EpisodeFile struct {
	ID           int64     `json:"id"`
	SeriesID     int64     `json:"seriesId"`
	SeasonNumber int       `json:"seasonNumber"`
	Size         httpx.Int `json:"size"`
	RelativePath string    `json:"relativePath"`
}

func idPath(prefix string, id int64) string { return prefix + strconv.FormatInt(id, 10) }

// SeriesByID returns one series, decoded, and its raw JSON (kept intact for
// PUT so that fields unknown to this client are preserved).
func (c *Client) SeriesByID(ctx context.Context, id int64) (*Series, json.RawMessage, error) {
	b, err := c.HTTP.Do(ctx, httpx.Request{Path: idPath("/api/v3/series/", id)})
	if err != nil {
		return nil, nil, err
	}
	var s Series
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, nil, fmt.Errorf("sonarr: decode series %d: %w", id, err)
	}
	return &s, b, nil
}

// Episodes lists a series' episodes.
func (c *Client) Episodes(ctx context.Context, seriesID int64) ([]Episode, error) {
	var out []Episode
	q := url.Values{"seriesId": {strconv.FormatInt(seriesID, 10)}}
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/episode", Query: q}, &out)
}

// EpisodeFiles lists a series' episode files.
func (c *Client) EpisodeFiles(ctx context.Context, seriesID int64) ([]EpisodeFile, error) {
	var out []EpisodeFile
	q := url.Values{"seriesId": {strconv.FormatInt(seriesID, 10)}}
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/episodefile", Query: q}, &out)
}

// DeleteEpisodeFiles deletes episode files from disk.
func (c *Client) DeleteEpisodeFiles(ctx context.Context, ids []int64) error {
	return c.HTTP.JSON(ctx, httpx.Request{Method: http.MethodDelete, Path: "/api/v3/episodefile/bulk",
		Body: map[string][]int64{"episodeFileIds": ids}}, nil)
}

// UpdateSeriesRaw sends a full series object (PUT /api/v3/series/{id}).
func (c *Client) UpdateSeriesRaw(ctx context.Context, id int64, raw json.RawMessage) error {
	return c.HTTP.JSON(ctx, httpx.Request{Method: http.MethodPut, Path: idPath("/api/v3/series/", id), Body: raw,
		Retry: true}, nil)
}

// DeleteSeries deletes a series (and its files when deleteFiles).
func (c *Client) DeleteSeries(ctx context.Context, id int64, deleteFiles, importListExclusion bool) error {
	q := url.Values{"deleteFiles": {strconv.FormatBool(deleteFiles)},
		"addImportListExclusion": {strconv.FormatBool(importListExclusion)}}
	return c.HTTP.JSON(ctx, httpx.Request{Method: http.MethodDelete, Path: idPath("/api/v3/series/", id), Query: q}, nil)
}

// UnmonitorSeason returns raw with seasons[seasonNumber].monitored = false,
// every other field untouched. ok is false when the season does not exist.
func UnmonitorSeason(raw json.RawMessage, seasonNumber int) (json.RawMessage, bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, err
	}
	var seasons []map[string]json.RawMessage
	if err := json.Unmarshal(obj["seasons"], &seasons); err != nil {
		return nil, false, err
	}
	found := false
	for _, s := range seasons {
		var n int
		if err := json.Unmarshal(s["seasonNumber"], &n); err == nil && n == seasonNumber {
			s["monitored"] = json.RawMessage("false")
			found = true
		}
	}
	if !found {
		return nil, false, nil
	}
	b, err := json.Marshal(seasons)
	if err != nil {
		return nil, false, err
	}
	obj["seasons"] = b
	out, err := json.Marshal(obj)
	return out, true, err
}

// DiskSpace is an entry of GET /api/v3/diskspace (shared by Sonarr and Radarr).
type DiskSpace struct {
	Path       string    `json:"path"`
	Label      string    `json:"label"`
	FreeSpace  httpx.Int `json:"freeSpace"`
	TotalSpace httpx.Int `json:"totalSpace"`
}

// DiskSpace lists the disks seen by Sonarr.
func (c *Client) DiskSpace(ctx context.Context) ([]DiskSpace, error) {
	var out []DiskSpace
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/diskspace"}, &out)
}

// CreateTag creates a tag (POST /api/v3/tag) and returns it with its id.
func (c *Client) CreateTag(ctx context.Context, label string) (*Tag, error) {
	return CreateTag(ctx, c.HTTP, label)
}

// CreateTag creates a tag on a Sonarr or Radarr instance.
func CreateTag(ctx context.Context, h *httpx.Client, label string) (*Tag, error) {
	var t Tag
	return &t, h.JSON(ctx, httpx.Request{Method: http.MethodPost, Path: "/api/v3/tag", Body: map[string]string{"label": label}}, &t)
}

// WithTags returns raw (a series or movie object) with its "tags" replaced;
// every other field is kept as is.
func WithTags(raw json.RawMessage, ids []int64) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []int64{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	obj["tags"] = b
	return json.Marshal(obj)
}

// HealthCheck is an entry of GET /api/v3/health (Sonarr and Radarr).
type HealthCheck struct {
	Source  string `json:"source"`
	Type    string `json:"type"` // ok | notice | warning | error
	Message string `json:"message"`
	WikiURL string `json:"wikiUrl"`
}

// Health lists the instance's health alerts.
func (c *Client) Health(ctx context.Context) ([]HealthCheck, error) { return Health(ctx, c.HTTP) }

// Health lists the health alerts of a Sonarr or Radarr instance.
func Health(ctx context.Context, h *httpx.Client) ([]HealthCheck, error) {
	var out []HealthCheck
	return out, h.JSON(ctx, httpx.Request{Path: "/api/v3/health"}, &out)
}

// CalendarEpisode is an entry of GET /api/v3/calendar (an episode with its
// series when includeSeries=true).
type CalendarEpisode struct {
	Episode
	FinaleType string  `json:"finaleType"` // series | season | midseason, Sonarr v4
	Series     *Series `json:"series"`
}

// Calendar lists the episodes airing in [start, end), monitored ones only.
func (c *Client) Calendar(ctx context.Context, start, end time.Time) ([]CalendarEpisode, error) {
	var out []CalendarEpisode
	q := url.Values{"start": {start.UTC().Format(time.RFC3339)}, "end": {end.UTC().Format(time.RFC3339)},
		"unmonitored": {"false"}, "includeSeries": {"true"}}
	return out, c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v3/calendar", Query: q}, &out)
}

// Indexer is an entry of GET /api/v3/indexer (Sonarr and Radarr). Its
// settings ("fields", with API keys) are deliberately not decoded.
type Indexer struct {
	ID                      int64  `json:"id"`
	Name                    string `json:"name"`
	Protocol                string `json:"protocol"` // usenet | torrent
	EnableRss               bool   `json:"enableRss"`
	EnableAutomaticSearch   bool   `json:"enableAutomaticSearch"`
	EnableInteractiveSearch bool   `json:"enableInteractiveSearch"`
	Priority                int    `json:"priority"`
}

// Indexers lists the instance's indexers.
func (c *Client) Indexers(ctx context.Context) ([]Indexer, error) { return Indexers(ctx, c.HTTP) }

// Indexers lists the indexers of a Sonarr or Radarr instance.
func Indexers(ctx context.Context, h *httpx.Client) ([]Indexer, error) {
	var out []Indexer
	return out, h.JSON(ctx, httpx.Request{Path: "/api/v3/indexer"}, &out)
}
