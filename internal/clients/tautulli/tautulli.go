// Package tautulli is a read client for the Tautulli API v2, the source of
// truth for watch history of every Plex user.
package tautulli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cleanarr/internal/clients/httpx"
)

// HistoryPageSize is the number of rows requested per get_history call.
const HistoryPageSize = 1000

// HistoryRow is one raw session (grouping=0).
type HistoryRow struct {
	ID                   httpx.Int    `json:"id"`
	ReferenceID          httpx.Int    `json:"reference_id"`
	Date                 httpx.Int    `json:"date"`
	Started              httpx.Int    `json:"started"`
	Stopped              httpx.Int    `json:"stopped"`
	Duration             httpx.Int    `json:"duration"`
	PlayDuration         httpx.Int    `json:"play_duration"`
	PausedCounter        httpx.Int    `json:"paused_counter"`
	UserID               httpx.Int    `json:"user_id"`
	User                 string       `json:"user"`
	FriendlyName         string       `json:"friendly_name"`
	Platform             string       `json:"platform"`
	Player               string       `json:"player"`
	MediaType            string       `json:"media_type"` // movie | episode | track…
	RatingKey            httpx.String `json:"rating_key"`
	ParentRatingKey      httpx.String `json:"parent_rating_key"`
	GrandparentRatingKey httpx.String `json:"grandparent_rating_key"`
	Title                string       `json:"title"`
	ParentTitle          string       `json:"parent_title"`
	GrandparentTitle     string       `json:"grandparent_title"`
	FullTitle            string       `json:"full_title"`
	Year                 httpx.Int    `json:"year"`
	MediaIndex           httpx.Int    `json:"media_index"`
	ParentMediaIndex     httpx.Int    `json:"parent_media_index"`
	PercentComplete      httpx.Int    `json:"percent_complete"`
	WatchedStatus        httpx.Float  `json:"watched_status"`
	TranscodeDecision    string       `json:"transcode_decision"`
}

// PlaySeconds is play_duration when present, else duration.
func (r HistoryRow) PlaySeconds() int64 {
	if r.PlayDuration > 0 {
		return int64(r.PlayDuration)
	}
	return int64(r.Duration)
}

// Watched reports whether Tautulli considers the session complete
// (watched_status = 1, i.e. above its watched threshold).
func (r HistoryRow) Watched() bool { return r.WatchedStatus >= 1 }

// HistoryPage is the data of a get_history response.
type HistoryPage struct {
	RecordsFiltered httpx.Int    `json:"recordsFiltered"`
	RecordsTotal    httpx.Int    `json:"recordsTotal"`
	Rows            []HistoryRow `json:"data"`
}

// User is an entry of get_users; user_id is the Plex account id.
type User struct {
	UserID       httpx.Int `json:"user_id"`
	Username     string    `json:"username"`
	FriendlyName string    `json:"friendly_name"`
	IsActive     httpx.Int `json:"is_active"`
	Thumb        string    `json:"thumb"`
}

// Client calls the Tautulli API.
type Client struct{ HTTP *httpx.Client }

// New builds a client; the API key travels in the apikey query parameter.
func New(baseURL, apiKey string) *Client {
	c := httpx.New("tautulli", baseURL, func(r *http.Request) {
		q := r.URL.Query()
		q.Set("apikey", apiKey)
		r.URL.RawQuery = q.Encode()
	})
	c.Timeout = 60 * time.Second // history pages can be slow
	return &Client{HTTP: c}
}

type envelope struct {
	Response struct {
		Result  string          `json:"result"`
		Message *string         `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"response"`
}

// APIError is a Tautulli response with result != "success".
type APIError struct {
	Cmd     string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("tautulli: %s: %s", e.Cmd, e.Message) }

// Call runs a command and decodes response.data into out.
func (c *Client) Call(ctx context.Context, cmd string, params url.Values, out any) error {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("cmd", cmd)
	var env envelope
	if err := c.HTTP.JSON(ctx, httpx.Request{Path: "/api/v2", Query: q}, &env); err != nil {
		return err
	}
	if env.Response.Result != "success" {
		msg := "unknown error"
		if env.Response.Message != nil {
			msg = *env.Response.Message
		}
		return &APIError{Cmd: cmd, Message: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Response.Data, out); err != nil {
		return fmt.Errorf("tautulli: %s: decode data: %w", cmd, err)
	}
	return nil
}

// History returns raw sessions, newest first.
func (c *Client) History(ctx context.Context, start, length int) (*HistoryPage, error) {
	var p HistoryPage
	err := c.Call(ctx, "get_history", url.Values{
		"grouping":     {"0"},
		"order_column": {"date"},
		"order_dir":    {"desc"},
		"start":        {strconv.Itoa(start)},
		"length":       {strconv.Itoa(length)},
	}, &p)
	return &p, err
}

// Users lists Plex users known to Tautulli.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out []User
	return out, c.Call(ctx, "get_users", nil, &out)
}

// Info checks the URL and the API key and returns the Tautulli version.
func (c *Client) Info(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"tautulli_version"`
	}
	err := c.Call(ctx, "get_tautulli_info", nil, &out)
	return out.Version, err
}
