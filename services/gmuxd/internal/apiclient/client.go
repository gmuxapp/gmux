// Package apiclient is a typed Go client for the gmuxd public HTTP API.
//
// It wraps the small number of endpoints a peer daemon (or any other
// programmatic client) needs: SSE event subscription, config and
// health fetch, session action forwarding, launch forwarding, and
// WebSocket proxying for terminal attachment.
//
// The goal is that any Go code talking to gmuxd goes through this
// client rather than hand-rolling http.Client calls. That keeps auth,
// read limits, timeouts, and keepalive policy in one place, and makes
// sure a fix in one consumer (e.g. the hub WS proxy read limit) also
// benefits every other consumer (the CLI, tests, future tools).
//
// Client is safe for concurrent use by multiple goroutines: each
// method builds its own http.Request against the shared http.Client
// and does not touch Client state.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sseclient"
)

// Read limits for ProxyWS. These match the values wsproxy.go uses
// for the equivalent browser-to-runner hop and fix the long-standing
// 256 KiB bug in peering.Peer.ProxyWS that caused reconnect loops on
// remote sessions with large terminals.
const (
	// wsClientReadLimit caps the browser → proxy direction. Browsers
	// only send keyboard input and resize messages, which are tiny.
	wsClientReadLimit = 256 * 1024

	// wsSpokeReadLimit caps the spoke → proxy direction. The spoke
	// relays terminal snapshots (full screen state with ANSI
	// attributes) on each attach; at 174x66 these reach ~1.3 MB, and
	// we give headroom to 4 MiB to match wsproxy's backend limit.
	wsSpokeReadLimit = 4 * 1024 * 1024
)

// httpActionTimeout bounds one-shot REST calls (GetConfig,
// ForwardAction, ForwardLaunch). Long enough for a slow spoke behind
// a VPN; short enough that a stuck peer doesn't hang a browser click.
const httpActionTimeout = 10 * time.Second

// Client is an authenticated HTTP + WebSocket client pointed at one
// gmuxd instance (the "spoke" from the hub's perspective).
type Client struct {
	baseURL   string
	token     string
	transport http.RoundTripper

	// httpClient is a long-lived client with no Timeout so SSE
	// subscriptions can run indefinitely. Short-lived REST calls use
	// per-request contexts with httpActionTimeout.
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBearerToken sets the Authorization: Bearer <token> header on
// every request. Empty token is ignored (tailscale-discovered peers
// authenticate via WhoIs instead).
func WithBearerToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithTransport sets the http.RoundTripper used for REST and WS
// calls. Used by tailscale-discovered peers to route through tsnet.
func WithTransport(t http.RoundTripper) Option {
	return func(c *Client) { c.transport = t }
}

// New creates a Client pointed at baseURL (e.g. "http://host:8790").
// A trailing slash on baseURL is tolerated and stripped.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
	}
	for _, opt := range opts {
		opt(c)
	}
	c.httpClient = &http.Client{Transport: c.transport}
	return c
}

// BaseURL returns the base URL this client was constructed with.
func (c *Client) BaseURL() string { return c.baseURL }

// setAuth adds the bearer token to a request if one is configured.
func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// Events returns an sseclient.Client for this spoke's /v1/events
// stream, preconfigured with auth and transport. The caller owns the
// reconnect loop; call Subscribe directly to start streaming.
func (c *Client) Events() *sseclient.Client {
	opts := []sseclient.Option{sseclient.WithBearerToken(c.token)}
	if c.transport != nil {
		opts = append(opts, sseclient.WithTransport(c.transport))
	}
	return sseclient.New(c.baseURL+"/v1/events", opts...)
}

// GetConfig fetches GET /v1/config and returns the Data field of the
// response envelope (the raw config JSON, unwrapped).
//
// Returns an error if the request fails, the status is not 200, the
// envelope cannot be decoded, or the response has ok=false.
func (c *Client) GetConfig(ctx context.Context) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, httpActionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/config", nil)
	if err != nil {
		return nil, fmt.Errorf("apiclient GetConfig: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apiclient GetConfig: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apiclient GetConfig: unexpected status %d", resp.StatusCode)
	}

	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("apiclient GetConfig: decode: %w", err)
	}
	if !envelope.OK {
		return nil, fmt.Errorf("apiclient GetConfig: ok=false")
	}
	return envelope.Data, nil
}

// ForwardAction proxies an HTTP request to the spoke's
// /v1/sessions/{sessionID}/{action} endpoint. It's the canonical way
// for a hub to implement kill/resume/dismiss/read/restart on a
// remote session.
//
// The sessionID passed here is the ORIGINAL (unnamespaced) session
// ID, not the hub's namespaced wire ID. The caller is responsible
// for stripping the "@peer" suffix before calling.
func (c *Client) ForwardAction(w http.ResponseWriter, r *http.Request, sessionID, action string) {
	path := fmt.Sprintf("/v1/sessions/%s/%s", sessionID, action)
	c.proxyHTTP(w, r, path)
}

// ForwardLaunch sends a launch request to the spoke's /v1/launch
// endpoint. The top-level "peer" field is stripped from the request
// body before forwarding, so the spoke treats the request as a local
// launch. Leaving the field in place would make the spoke try to
// forward the request again to a peer of its own with that name
// (which typically doesn't exist on that side).
//
// Returns an HTTP error to the client on invalid JSON, I/O errors,
// or upstream failures.
func (c *Client) ForwardLaunch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stripped, err := stripPeerField(body)
	if err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(stripped))
	r.ContentLength = int64(len(stripped))
	c.proxyHTTP(w, r, "/v1/launch")
}

// stripPeerField removes the top-level "peer" key from a JSON object
// body. Kept as a package-level function for unit testability.
func stripPeerField(body []byte) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	delete(req, "peer")
	return json.Marshal(req)
}

// proxyHTTP forwards an HTTP request to the spoke at the given path
// and copies the response back to the caller, preserving method,
// body, status, headers, and Content-Type.
func (c *Client) proxyHTTP(w http.ResponseWriter, r *http.Request, path string) {
	ctx, cancel := context.WithTimeout(r.Context(), httpActionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, c.baseURL+path, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.setAuth(req)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("apiclient: forward %s: %v", path, err)
		http.Error(w, "peer unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// DialWS opens a WebSocket connection to the spoke's /ws/{sessionID}
// endpoint, injecting bearer auth and honoring the client's transport.
// The caller owns the returned connection and must close it.
func (c *Client) DialWS(ctx context.Context, sessionID string) (*websocket.Conn, error) {
	base := strings.TrimRight(c.baseURL, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + base[len("https://"):]
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + base[len("http://"):]
	}
	spokeURL := fmt.Sprintf("%s/ws/%s", base, sessionID)

	dialOpts := &websocket.DialOptions{}
	if c.token != "" {
		dialOpts.HTTPHeader = http.Header{
			"Authorization": []string{"Bearer " + c.token},
		}
	}
	if c.transport != nil {
		dialOpts.HTTPClient = &http.Client{Transport: c.transport}
	}

	conn, _, err := websocket.Dial(ctx, spokeURL, dialOpts)
	if err != nil {
		return nil, fmt.Errorf("apiclient DialWS: %w", err)
	}
	return conn, nil
}

// ProxyWS accepts the incoming browser WebSocket upgrade and proxies
// it to the spoke's /ws/{sessionID}. It handles:
//
//   - Accept + Dial of the two WS connections.
//   - Direction-specific read limits (4 MiB spoke → client for large
//     terminal snapshots, 256 KiB client → spoke for keyboard input).
//   - Bidirectional copy with shared cancellation when either side
//     closes or errors.
//   - Clean shutdown of both connections on exit.
//
// sessionID is the ORIGINAL (unnamespaced) session ID on the spoke,
// not the namespaced wire ID. Callers strip the suffix first.
func (c *Client) ProxyWS(w http.ResponseWriter, r *http.Request, sessionID string) {
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("apiclient ProxyWS: accept: %v", err)
		return
	}

	spokeConn, err := c.DialWS(r.Context(), sessionID)
	if err != nil {
		log.Printf("apiclient ProxyWS: dial %s: %v", sessionID, err)
		clientConn.Close(websocket.StatusInternalError, "peer unavailable")
		return
	}

	// Direction-specific limits: browser input is tiny, spoke output
	// carries full terminal snapshots.
	clientConn.SetReadLimit(wsClientReadLimit)
	spokeConn.SetReadLimit(wsSpokeReadLimit)

	c.pipeWS(r.Context(), clientConn, spokeConn, sessionID)
}

// pipeWS runs the bidirectional copy loop between an already-accepted
// client connection and an already-dialed spoke connection, blocking
// until one side closes. Extracted from ProxyWS so tests can exercise
// the copy loop without a real HTTP upgrade.
func (c *Client) pipeWS(parent context.Context, clientConn, spokeConn *websocket.Conn, sessionID string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// Spoke → Client (terminal output + resize events).
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := spokeConn.Read(ctx)
			if err != nil {
				return
			}
			if err := clientConn.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}()

	// Client → Spoke (keyboard input + resize).
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := clientConn.Read(ctx)
			if err != nil {
				return
			}
			if err := spokeConn.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}()

	wg.Wait()

	clientConn.Close(websocket.StatusNormalClosure, "")
	spokeConn.Close(websocket.StatusNormalClosure, "")
	log.Printf("apiclient ProxyWS: %s disconnected", sessionID)
}
