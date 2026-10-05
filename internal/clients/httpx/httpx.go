// Package httpx is the small HTTP layer shared by the upstream API clients:
// per-request timeout, retries with backoff on transient failures, bounded
// response bodies, and error messages that never contain credentials.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults for every client.
const (
	DefaultTimeout = 30 * time.Second
	DefaultRetries = 2
	DefaultBackoff = 500 * time.Millisecond
	// MaxBodyBytes caps a response; large Plex libraries stay well below.
	MaxBodyBytes = 256 << 20
)

// Client talks to one upstream service.
type Client struct {
	Name    string // used in errors and logs: "sonarr", "plex"…
	BaseURL string
	HTTP    *http.Client
	// Auth adds credentials to a request (header or query).
	Auth    func(r *http.Request)
	Timeout time.Duration
	Retries int
	Backoff time.Duration
}

// maxRedirects bounds the redirects followed on the same host.
const maxRedirects = 5

// SameHostOnly is the redirect policy of the upstream clients: Go forwards
// custom headers (X-Api-Key, X-Plex-Token) to any redirect target, so a
// redirect to another host is refused instead of leaking the credential.
func SameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Hostname() != via[0].URL.Hostname() {
		return &RedirectError{Host: req.URL.Hostname()}
	}
	return nil
}

// RedirectError is a refused redirect to another host.
type RedirectError struct{ Host string }

func (e *RedirectError) Error() string {
	return fmt.Sprintf("redirect to another host (%s) refused: check the URL", e.Host)
}

// defaultHTTP is shared by every upstream client.
var defaultHTTP = &http.Client{CheckRedirect: SameHostOnly}

// New returns a client with default timeout and retry policy.
func New(name, baseURL string, auth func(*http.Request)) *Client {
	return &Client{
		Name: name, BaseURL: strings.TrimRight(baseURL, "/"), HTTP: defaultHTTP, Auth: auth,
		Timeout: DefaultTimeout, Retries: DefaultRetries, Backoff: DefaultBackoff,
	}
}

// StatusError is returned for non-2xx responses.
type StatusError struct {
	Service string
	Method  string
	Path    string
	Status  int
	Body    string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s: %s %s: HTTP %d", e.Service, e.Method, e.Path, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// Request describes one call. Query values are never echoed in errors.
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Header  map[string]string
	Body    any // JSON-encoded when not nil
	Timeout time.Duration
}

// Do performs the request and returns the response body.
func (c *Client) Do(ctx context.Context, req Request) ([]byte, error) {
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	var payload []byte
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("%s: encode body: %w", c.Name, err)
		}
		payload = b
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = c.Timeout
	}
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			wait := c.Backoff << (attempt - 1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		body, status, err := c.once(ctx, req, payload, timeout)
		if err == nil && status/100 == 2 {
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %s %s: %w", c.Name, req.Method, req.Path, err)
			continue
		}
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		lastErr = &StatusError{Service: c.Name, Method: req.Method, Path: req.Path, Status: status, Body: snippet}
		if !retryable(status) {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, req Request, payload []byte, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := c.BaseURL + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	r, err := http.NewRequestWithContext(ctx, req.Method, u, body)
	if err != nil {
		return nil, 0, errors.New("invalid request")
	}
	r.Header.Set("Accept", "application/json")
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.Header {
		r.Header.Set(k, v)
	}
	if c.Auth != nil {
		c.Auth(r)
	}
	res, err := c.HTTP.Do(r)
	if err != nil {
		// url.Error embeds the full URL, which may carry an API key.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, res.StatusCode, err
	}
	if len(b) > MaxBodyBytes {
		return nil, res.StatusCode, fmt.Errorf("response larger than %d bytes", MaxBodyBytes)
	}
	return b, res.StatusCode, nil
}

// JSON performs the request and decodes the JSON response into out.
func (c *Client) JSON(ctx context.Context, req Request, out any) error {
	b, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: %s %s: decode response: %w", c.Name, req.Method, req.Path, err)
	}
	return nil
}
