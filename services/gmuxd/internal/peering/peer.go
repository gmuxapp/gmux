package peering

import (
	"bufio"
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

	mu     sync.RWMutex
	status Status

	client *http.Client
}

func newPeer(cfg config.PeerConfig, st *store.Store) *Peer {
	return &Peer{
		Config: cfg,
		store:  st,
		status: StatusDisconnected,
		client: &http.Client{Timeout: 0}, // SSE: no timeout on the response body
	}
}

// Status returns the current connection state.
func (p *Peer) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

func (p *Peer) setStatus(s Status) {
	p.mu.Lock()
	p.status = s
	p.mu.Unlock()
}

// Forward proxies an HTTP request to the spoke, stripping the peer
// namespace from the session ID. The spoke sees the original session ID.
func (p *Peer) Forward(w http.ResponseWriter, r *http.Request, originalID, action string) {
	path := fmt.Sprintf("/v1/sessions/%s/%s", originalID, action)
	p.proxyHTTP(w, r, path)
}

// ForwardLaunch sends a launch request to the spoke.
func (p *Peer) ForwardLaunch(w http.ResponseWriter, r *http.Request) {
	p.proxyHTTP(w, r, "/v1/launch")
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
	spokeConn, _, err := websocket.Dial(ctx, spokeURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + p.Config.Token},
		},
	})
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
	req.Header.Set("Authorization", "Bearer "+p.Config.Token)
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
	req.Header.Set("Authorization", "Bearer "+p.Config.Token)

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
		namespacedID := NamespaceID(ev.ID, p.Config.Name)
		p.store.Remove(namespacedID)

	case "session-activity":
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
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
