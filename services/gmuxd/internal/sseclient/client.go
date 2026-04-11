// Package sseclient is a minimal Server-Sent Events client.
//
// It handles the SSE wire format (event:/data:/comment lines), auth
// headers, and buffer sizing. Reconnect policy is intentionally NOT
// part of this package: callers own the outer loop, mirroring how
// browser EventSource tracks reconnection in the caller.
//
// Usage:
//
//	c := sseclient.New("http://host:8790/v1/events",
//	    sseclient.WithBearerToken(token),
//	)
//	err := c.Subscribe(ctx, nil, func(ev sseclient.Event) {
//	    // dispatch ev.Type / ev.Data
//	})
//	// err is ErrStreamEnded on clean close, ErrUnauthorized on auth
//	// failure, or a wrapped network/protocol error otherwise.
package sseclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Default buffer size for SSE event decoding. Matches the size used
// by peering's hand-rolled scanner prior to extraction. Large enough
// to accept gmuxd session-upsert payloads with long command arrays.
const defaultBufferSize = 256 * 1024

// ErrStreamEnded is returned by Subscribe when the server closed the
// stream cleanly (no error from the scanner, EOF). Callers use this
// as the trigger to reconnect with backoff.
var ErrStreamEnded = errors.New("sse stream ended")

// ErrUnauthorized is returned by Subscribe when the server responds
// with HTTP 401 or 403. Callers typically do not retry this.
var ErrUnauthorized = errors.New("sse unauthorized")

// Event is a decoded SSE event. Data is the raw bytes from one or
// more "data:" lines, concatenated with newlines (per the spec).
// Callers parse Data according to their own schema.
type Event struct {
	Type string
	Data []byte
}

// Client subscribes to a single SSE endpoint.
//
// Client is safe to reuse across multiple Subscribe calls on the same
// URL (e.g. after a reconnect) but not safe for concurrent Subscribe
// calls from multiple goroutines.
type Client struct {
	url       string
	headers   http.Header
	transport http.RoundTripper
	bufSize   int
}

// Option configures a Client.
type Option func(*Client)

// WithBearerToken adds an Authorization: Bearer <token> header to
// every request. Empty token is ignored (useful for tailscale
// connections that authenticate via WhoIs instead).
func WithBearerToken(token string) Option {
	return func(c *Client) {
		if token != "" {
			c.headers.Set("Authorization", "Bearer "+token)
		}
	}
}

// WithHeader adds a custom HTTP header. Repeated calls append to the
// same header name.
func WithHeader(key, value string) Option {
	return func(c *Client) {
		c.headers.Add(key, value)
	}
}

// WithTransport sets the HTTP round-tripper used for the connection.
// Used by tailscale-discovered peers to route through tsnet.
func WithTransport(t http.RoundTripper) Option {
	return func(c *Client) {
		c.transport = t
	}
}

// WithBufferSize sets the maximum size of a single SSE event payload.
// Defaults to 256 KiB. Events larger than this cause Subscribe to
// return a protocol error.
func WithBufferSize(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.bufSize = n
		}
	}
}

// New creates a Client pointed at url.
func New(url string, opts ...Option) *Client {
	c := &Client{
		url:     url,
		headers: make(http.Header),
		bufSize: defaultBufferSize,
	}
	c.headers.Set("Accept", "text/event-stream")
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Subscribe connects to the SSE endpoint and invokes handler for
// each decoded event until the context is cancelled or the stream
// ends.
//
// connected, if non-nil, is called exactly once after a successful
// HTTP response (2xx) and before the first handler call. Callers use
// this to transition their connection state to Connected or to fetch
// auxiliary resources (config, etc.) on connection.
//
// Return values:
//   - ErrStreamEnded: the server closed the stream without error.
//   - ErrUnauthorized: the server responded with 401 or 403.
//   - context.Canceled / DeadlineExceeded: ctx was cancelled.
//   - wrapped network/protocol error: everything else.
//
// Comment lines (lines starting with ':') are consumed silently.
// Lines that don't match "event: " or "data: " are ignored, per the
// SSE spec.
//
// Events with no "data:" line are silently dropped. Lines with
// "data:" but no preceding "event:" are also dropped (the current
// event type is required to dispatch).
func (c *Client) Subscribe(ctx context.Context, connected func(), handler func(Event)) error {
	if handler == nil {
		panic("sseclient: handler must not be nil")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("sse request: %w", err)
	}
	for k, vv := range c.headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	// Fresh http.Client per Subscribe so Timeout doesn't apply to the
	// long-lived stream read. Caller can still cancel via ctx.
	httpClient := &http.Client{Transport: c.transport}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sse connect: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("sse unexpected status %d", resp.StatusCode)
	}

	if connected != nil {
		connected()
	}

	scanner := bufio.NewScanner(resp.Body)
	// bufio.Scanner uses max(initial cap, configured max) as the real
	// limit, so the initial cap must not exceed bufSize or the max is
	// a no-op for small bufSize values.
	initSize := 64 * 1024
	if c.bufSize < initSize {
		initSize = c.bufSize
	}
	scanner.Buffer(make([]byte, 0, initSize), c.bufSize)

	var currentEvent string
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}

		line := scanner.Text()
		switch {
		case line == "":
			// Spec: empty line terminates event. We already dispatch on
			// data: lines, so this is a no-op. Reset currentEvent to
			// avoid stale dispatches across event boundaries.
			currentEvent = ""
		case strings.HasPrefix(line, ":"):
			// Comment line (server-pushed keepalive). Ignore.
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "event:"):
			// Spec-compliant form without the space.
			currentEvent = strings.TrimPrefix(line, "event:")
		case strings.HasPrefix(line, "data: "):
			if currentEvent == "" {
				continue
			}
			handler(Event{Type: currentEvent, Data: []byte(strings.TrimPrefix(line, "data: "))})
		case strings.HasPrefix(line, "data:"):
			if currentEvent == "" {
				continue
			}
			handler(Event{Type: currentEvent, Data: []byte(strings.TrimPrefix(line, "data:"))})
		default:
			// Unknown line type (id:, retry:, custom). Ignore per spec.
		}
	}

	if err := scanner.Err(); err != nil {
		// ctx.Err() takes priority: if the caller cancelled, surface
		// that directly rather than the ambient "context canceled"
		// wrapped in a read error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("sse read: %w", err)
	}

	return ErrStreamEnded
}
