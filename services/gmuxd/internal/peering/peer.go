package peering

import (
	"bufio"
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

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"nhooyr.io/websocket"
)

// Peer manages the connection to a single remote gmuxd instance.
type Peer struct {
	Config config.PeerConfig
	store  *store.Store

	mu           sync.RWMutex
	status       Status
	lastError    string          // human-readable reason for last disconnect
	cachedConfig json.RawMessage // peer's /v1/config data, fetched on connect

	// onStatus is called when connection state changes.
	onStatus func(name string, status Status)

	// isKnownOrigin reports whether a peer name refers to this node or
	// another peer we're directly connected to. Used to drop forwarded
	// sessions that we can reach via a shorter path (or that are our
	// own sessions echoed back through a mutual subscription).
	isKnownOrigin func(name string) bool

	// transport is the HTTP round-tripper for all spoke connections.
	// nil means use the default transport. Set via WithTransport for
	// tailscale-discovered peers that route through tsnet.
	transport http.RoundTripper

	client *http.Client
}

func newPeer(cfg config.PeerConfig, st *store.Store, onStatus func(string, Status), opts ...PeerOption) *Peer {
	p := &Peer{
		Config:   cfg,
		store:    st,
		status:   StatusDisconnected,
		onStatus: onStatus,
		client:   &http.Client{Timeout: 0},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Status returns the current connection state.
func (p *Peer) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

// LastError returns a human-readable reason for the last disconnect.
func (p *Peer) LastError() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastError
}

func (p *Peer) setStatus(s Status) {
	p.mu.Lock()
	old := p.status
	p.status = s
	if s == StatusConnected {
		p.lastError = ""
	}
	p.mu.Unlock()

	if old != s && p.onStatus != nil {
		p.onStatus(p.Config.Name, s)
	}
}

// Forward proxies an HTTP request to the spoke, stripping the peer
// namespace from the session ID. The spoke sees the original session ID.
func (p *Peer) Forward(w http.ResponseWriter, r *http.Request, originalID, action string) {
	path := fmt.Sprintf("/v1/sessions/%s/%s", originalID, action)
	p.proxyHTTP(w, r, path)
}

// ForwardLaunch sends a launch request to the spoke. The request body is
// expected to be JSON matching the /v1/launch schema. Any top-level "peer"
// field is stripped before forwarding so the spoke treats the request as
// a local launch; leaving it in place would make the spoke try to forward
// the request again to a peer of its own with that name (which typically
// doesn't exist on that side).
func (p *Peer) ForwardLaunch(w http.ResponseWriter, r *http.Request) {
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
	p.proxyHTTP(w, r, "/v1/launch")
}

// stripPeerField removes the top-level "peer" key from a JSON object body.
// Exported as a function (not a method) so it can be unit-tested in
// isolation; callers generally go through ForwardLaunch.
func stripPeerField(body []byte) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	delete(req, "peer")
	return json.Marshal(req)
}

// CachedConfig returns the peer's config data, fetched once on each
// successful SSE connection. Returns nil if the peer is not connected
// or the config has not been fetched yet.
func (p *Peer) CachedConfig() json.RawMessage {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cachedConfig
}

// fetchConfig fetches the spoke's /v1/config and caches the result.
// Called once after each successful SSE connection.
func (p *Peer) fetchConfig(ctx context.Context) {
	url := strings.TrimRight(p.Config.URL, "/") + "/v1/config"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	p.setAuth(req)

	client := &http.Client{Timeout: 5 * time.Second, Transport: p.transport}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("peering: %s: fetch config: %v", p.Config.Name, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil || !envelope.OK {
		return
	}

	p.mu.Lock()
	p.cachedConfig = envelope.Data
	p.mu.Unlock()
}

// ProxyWS proxies a browser WebSocket connection to the spoke's
// /ws/{sessionID} endpoint. The hub accepts the browser WS, dials the
// spoke WS with bearer auth, and pipes bidirectionally.
func (p *Peer) ProxyWS(w http.ResponseWriter, r *http.Request, originalID string) {
	// Accept browser WebSocket.
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("peering: %s: ws accept: %v", p.Config.Name, err)
		return
	}

	// Dial spoke's WebSocket.
	base := strings.TrimRight(p.Config.URL, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + base[len("https://"):]
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + base[len("http://"):]
	}
	spokeURL := fmt.Sprintf("%s/ws/%s", base, originalID)
	ctx := r.Context()
	dialOpts := &websocket.DialOptions{}
	if p.Config.Token != "" {
		dialOpts.HTTPHeader = http.Header{
			"Authorization": []string{"Bearer " + p.Config.Token},
		}
	}
	if p.transport != nil {
		dialOpts.HTTPClient = &http.Client{Transport: p.transport}
	}
	spokeConn, _, err := websocket.Dial(ctx, spokeURL, dialOpts)
	if err != nil {
		log.Printf("peering: %s: ws dial %s: %v", p.Config.Name, originalID, err)
		clientConn.Close(websocket.StatusInternalError, "peer unavailable")
		return
	}

	log.Printf("peering: %s: ws proxying %s", p.Config.Name, originalID)

	// Match read limits with the main WS proxy.
	clientConn.SetReadLimit(256 * 1024)
	spokeConn.SetReadLimit(256 * 1024)

	proxyCtx, proxyCancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(2)

	// Spoke → Client
	go func() {
		defer wg.Done()
		defer proxyCancel()
		for {
			typ, data, err := spokeConn.Read(proxyCtx)
			if err != nil {
				return
			}
			if err := clientConn.Write(proxyCtx, typ, data); err != nil {
				return
			}
		}
	}()

	// Client → Spoke
	go func() {
		defer wg.Done()
		defer proxyCancel()
		for {
			typ, data, err := clientConn.Read(proxyCtx)
			if err != nil {
				return
			}
			if err := spokeConn.Write(proxyCtx, typ, data); err != nil {
				return
			}
		}
	}()

	wg.Wait()

	clientConn.Close(websocket.StatusNormalClosure, "")
	spokeConn.Close(websocket.StatusNormalClosure, "")
	log.Printf("peering: %s: ws disconnected %s", p.Config.Name, originalID)
}

// proxyHTTP forwards an HTTP request to the spoke at the given path
// and copies the response back to the caller.
func (p *Peer) proxyHTTP(w http.ResponseWriter, r *http.Request, path string) {
	targetURL := strings.TrimRight(p.Config.URL, "/") + path

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.setAuth(req)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("peering: %s: forward %s: %v", p.Config.Name, path, err)
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
	io.Copy(w, resp.Body)
}

// run connects to the spoke's SSE stream and processes events until the
// context is cancelled. Handles reconnection with exponential backoff.
func (p *Peer) run(ctx context.Context) {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		p.setStatus(StatusConnecting)
		wasConnected := false
		err := p.subscribe(ctx, func() { wasConnected = true })

		// Clean up spoke sessions from store on disconnect.
		removed := p.store.RemoveByPeer(p.Config.Name)
		if len(removed) > 0 {
			log.Printf("peering: %s: removed %d sessions on disconnect", p.Config.Name, len(removed))
		}

		if err != nil && ctx.Err() == nil {
			p.mu.Lock()
			p.lastError = categorizeError(err)
			p.mu.Unlock()
		}
		p.mu.Lock()
		p.cachedConfig = nil
		p.mu.Unlock()
		p.setStatus(StatusDisconnected)

		if ctx.Err() != nil {
			return
		}

		// Reset backoff after a successful connection so transient drops
		// reconnect quickly instead of carrying over stale backoff.
		if wasConnected {
			backoff = initialBackoff
		}

		log.Printf("peering: %s: disconnected: %v (retry in %s)", p.Config.Name, err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// subscribe connects to the spoke and processes its SSE stream.
// The onConnected callback fires once after a successful connection,
// allowing the caller to track whether the connection was established
// (used to decide whether to reset backoff).
func (p *Peer) subscribe(ctx context.Context, onConnected func()) error {
	url := fmt.Sprintf("%s/v1/events", strings.TrimRight(p.Config.URL, "/"))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("auth failed (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	p.setStatus(StatusConnected)
	if onConnected != nil {
		onConnected()
	}
	log.Printf("peering: %s: connected to %s", p.Config.Name, url)

	// Fetch the peer's config once per connection so /v1/config can
	// serve it from cache without making outgoing HTTP calls.
	p.fetchConfig(ctx)

	scanner := bufio.NewScanner(resp.Body)
	// Allow large SSE payloads (sessions can have long command arrays).
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)

	var currentEvent string

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if currentEvent != "" {
				p.handleEvent(currentEvent, []byte(data))
				currentEvent = ""
			}
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	return fmt.Errorf("stream ended")
}

// sseEvent is the wire format for gmuxd SSE events.
type sseEvent struct {
	Type    string           `json:"type"`
	ID      string           `json:"id"`
	Session *json.RawMessage `json:"session,omitempty"`
}

// isForwardedFromKnownOrigin checks whether a session ID (before
// namespacing) was forwarded from a peer we can reach directly.
// Returns true if the session should be dropped.
func (p *Peer) isForwardedFromKnownOrigin(id string) bool {
	if p.isKnownOrigin == nil {
		return false
	}
	_, innerPeer := ParseID(id)
	return innerPeer != "" && p.isKnownOrigin(innerPeer)
}

func (p *Peer) handleEvent(eventType string, data []byte) {
	switch eventType {
	case "session-upsert":
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("peering: %s: bad upsert event: %v", p.Config.Name, err)
			return
		}
		if ev.Session == nil {
			return
		}
		if p.isForwardedFromKnownOrigin(ev.ID) {
			return
		}
		var sess store.Session
		if err := json.Unmarshal(*ev.Session, &sess); err != nil {
			log.Printf("peering: %s: bad session payload: %v", p.Config.Name, err)
			return
		}

		// Transform for local store.
		sess.ID = NamespaceID(ev.ID, p.Config.Name)
		sess.Peer = p.Config.Name
		sess.SocketPath = "" // meaningless on hub side

		p.store.Upsert(sess)

	case "session-remove":
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("peering: %s: bad remove event: %v", p.Config.Name, err)
			return
		}
		if p.isForwardedFromKnownOrigin(ev.ID) {
			return
		}
		namespacedID := NamespaceID(ev.ID, p.Config.Name)
		p.store.Remove(namespacedID)

	case "session-activity":
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return
		}
		if p.isForwardedFromKnownOrigin(ev.ID) {
			return
		}
		namespacedID := NamespaceID(ev.ID, p.Config.Name)
		p.store.Broadcast(store.Event{
			Type: "session-activity",
			ID:   namespacedID,
		})

	case "projects-update":
		// Ignore: hub has its own projects.

	default:
		// Unknown event types are silently ignored for forward compatibility.
	}
}

// setAuth adds the bearer token to a request if one is configured.
// Tailscale-discovered peers authenticate via WhoIs identity and have
// no token; manual and devcontainer peers use bearer tokens.
func (p *Peer) setAuth(req *http.Request) {
	if p.Config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Config.Token)
	}
}

// categorizeError returns a short, user-friendly description of a peer
// connection failure. Intended for display in the UI, not for logs.
func categorizeError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "auth failed"):
		return "authentication failed"
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "no such host"):
		return "host not found"
	case strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "context deadline exceeded"):
		return "connection timed out"
	case strings.Contains(s, "certificate"),
		strings.Contains(s, "x509"):
		return "TLS certificate error"
	case strings.Contains(s, "stream ended"):
		return "connection lost"
	default:
		return "connection failed"
	}
}
