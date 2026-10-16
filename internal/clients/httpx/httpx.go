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

	"hangarr/internal/media"
)

// Defaults for every client.
const (
	DefaultTimeout = 30 * time.Second
	// DefaultDeleteTimeout bounds a DELETE: Sonarr/Radarr may move the files
	// to a recycle bin on another file system, which takes minutes.
	DefaultDeleteTimeout = 5 * time.Minute
	DefaultRetries       = 2
	DefaultBackoff       = 500 * time.Millisecond
	// MaxBodyBytes caps a response; large Plex libraries stay well below.
	MaxBodyBytes = 256 << 20
)

// Client talks to one upstream service.
type Client struct {
	Name    string // used in errors and logs: "sonarr", "plex"…
	BaseURL string
	HTTP    *http.Client
	// Auth adds credentials to a request (header or query).
	Auth          func(r *http.Request)
	Timeout       time.Duration
	DeleteTimeout time.Duration // timeout of a DELETE (DefaultDeleteTimeout when 0)
	Retries       int
	Backoff       time.Duration
}

// maxRedirects bounds the redirects followed on the same host.
const maxRedirects = 5

// SameHostOnly is the redirect policy of the upstream clients: Go forwards
// custom headers (X-Api-Key, X-Plex-Token) to any redirect target, so a
// redirect to another origin (scheme, host and port) is refused instead of
// leaking the credential to another service, and so is a downgrade from
// https to http (the credential would travel in clear). The only change
// allowed is http to https on the same host, to port 443 from port 80 (or 443).
func SameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	from, to := via[0].URL, req.URL
	if !strings.EqualFold(to.Hostname(), from.Hostname()) {
		return &RedirectError{Host: to.Hostname()}
	}
	if from.Scheme == "https" && to.Scheme != "https" {
		return &RedirectError{Host: to.Hostname(), Downgrade: true}
	}
	if from.Scheme == to.Scheme && effectivePort(from) == effectivePort(to) {
		return nil
	}
	if from.Scheme == "http" && to.Scheme == "https" && effectivePort(to) == "443" &&
		(effectivePort(from) == "80" || effectivePort(from) == "443") {
		return nil
	}
	return &RedirectError{Host: to.Host}
}

// effectivePort is the port of u, explicit or implied by its scheme.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// RedirectError is a refused redirect to another origin, or to plain http.
type RedirectError struct {
	Host      string
	Downgrade bool // https to http on the same host
}

func (e *RedirectError) Error() string {
	if e.Downgrade {
		return fmt.Sprintf("redirect from https to http (%s) refused: check the URL", e.Host)
	}
	return fmt.Sprintf("redirect to another host or port (%s) refused: check the URL", e.Host)
}

// defaultHTTP is shared by every upstream client.
var defaultHTTP = &http.Client{CheckRedirect: SameHostOnly}

// New returns a client with default timeout and retry policy.
func New(name, baseURL string, auth func(*http.Request)) *Client {
	return &Client{
		Name: name, BaseURL: strings.TrimRight(baseURL, "/"), HTTP: defaultHTTP, Auth: auth,
		Timeout: DefaultTimeout, DeleteTimeout: DefaultDeleteTimeout, Retries: DefaultRetries, Backoff: DefaultBackoff,
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

// IsNotFound reports an HTTP 404 answer.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// Ambiguous reports an error after which the upstream may have performed the
// request anyway: a timeout or a connection cut, not an HTTP answer, a
// refused redirect or a request that could not be built. A deletion that
// ends so must be checked before it is reported as failed.
func Ambiguous(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errInvalidRequest) {
		return false
	}
	var se *StatusError
	var re *RedirectError
	var te *TooLargeError
	return !errors.As(err, &se) && !errors.As(err, &re) && !errors.As(err, &te)
}

// TooLargeError is a response above the size limit of the request.
type TooLargeError struct{ Limit int64 }

func (e *TooLargeError) Error() string { return fmt.Sprintf("response larger than %d bytes", e.Limit) }

var errInvalidRequest = errors.New("invalid request")

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
	// Retry allows retries of a method other than GET and HEAD, which are
	// the only ones retried by default: a repeated DELETE or POST may act
	// twice (a second tag, a 404 for a deletion that succeeded). Set it only
	// for a request that is safe to repeat, such as a PUT of a whole object.
	Retry bool
	// MaxBytes caps the response (MaxBodyBytes when 0); a larger one fails
	// without being read further.
	MaxBytes int64
}

func (req Request) retries(c *Client) int {
	if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Retry {
		return c.Retries
	}
	return 0
}

func (req Request) timeout(c *Client) time.Duration {
	switch {
	case req.Timeout > 0:
		return req.Timeout
	case req.Method == http.MethodDelete && c.DeleteTimeout > 0:
		return c.DeleteTimeout
	case req.Method == http.MethodDelete:
		return DefaultDeleteTimeout
	}
	return c.Timeout
}

// Do performs the request and returns the response body.
func (c *Client) Do(ctx context.Context, req Request) ([]byte, error) {
	var out []byte
	err := c.send(ctx, req, func(r io.Reader) error {
		b, err := io.ReadAll(r)
		out = b
		return err
	})
	return out, err
}

// JSON performs the request and decodes the JSON response into out, as it
// arrives (nil out: the body is read and dropped).
func (c *Client) JSON(ctx context.Context, req Request, out any) error {
	return c.send(ctx, req, func(r io.Reader) error {
		if out == nil {
			_, err := io.Copy(io.Discard, r)
			return err
		}
		if err := json.NewDecoder(r).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	})
}

// send performs the request with retries; consume reads a 2xx body.
func (c *Client) send(ctx context.Context, req Request, consume func(io.Reader) error) error {
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	var payload []byte
	if req.Body != nil {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return fmt.Errorf("%s: encode body: %w", c.Name, err)
		}
		payload = b
	}
	timeout, retries := req.timeout(c), req.retries(c)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			wait := c.Backoff << (attempt - 1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		status, snippet, err, consumeErr := c.once(ctx, req, payload, timeout, consume)
		if consumeErr != nil {
			return fmt.Errorf("%s: %s %s: %w", c.Name, req.Method, req.Path, consumeErr)
		}
		if err == nil && status/100 == 2 {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %s %s: %w", c.Name, req.Method, req.Path, err)
			var re *RedirectError
			if errors.As(err, &re) || errors.Is(err, errInvalidRequest) {
				break
			}
			continue
		}
		lastErr = &StatusError{Service: c.Name, Method: req.Method, Path: req.Path, Status: status, Body: snippet}
		if !retryable(status) {
			break
		}
	}
	return lastErr
}

// snippetBytes is how much of an error response is kept for the message.
const snippetBytes = 200

// once sends the request one time. err is a transport error (retried for
// idempotent requests), consumeErr an error of consume on a 2xx body (not
// retried, unless the body could not be read).
func (c *Client) once(ctx context.Context, req Request, payload []byte, timeout time.Duration,
	consume func(io.Reader) error) (status int, snippet string, err, consumeErr error) {
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
		return 0, "", errInvalidRequest, nil
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
		return 0, "", err, nil
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4*snippetBytes))
		return res.StatusCode, media.Ellipsis(strings.TrimSpace(string(b)), snippetBytes), nil, nil
	}
	limit := req.MaxBytes
	if limit <= 0 {
		limit = MaxBodyBytes
	}
	cr := &cappedReader{r: res.Body, left: limit, limit: limit}
	if err := consume(cr); err != nil {
		if cr.readErr != nil {
			return res.StatusCode, "", cr.readErr, nil // transport failure while reading
		}
		return res.StatusCode, "", nil, err
	}
	return res.StatusCode, "", nil, nil
}

// cappedReader fails with *TooLargeError past limit bytes and records the
// transport errors of the body.
type cappedReader struct {
	r       io.Reader
	left    int64
	limit   int64
	readErr error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		if n, err := c.r.Read(one[:]); n > 0 {
			return 0, &TooLargeError{Limit: c.limit}
		} else if err != nil {
			return 0, c.note(err)
		}
		return 0, nil
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, c.note(err)
}

func (c *cappedReader) note(err error) error {
	if err != nil && !errors.Is(err, io.EOF) {
		c.readErr = err
	}
	return err
}
