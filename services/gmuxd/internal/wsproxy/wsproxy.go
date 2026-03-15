// Package wsproxy provides a WebSocket reverse proxy from gmuxd to gmux-run
// session sockets. Browser connects to gmuxd /ws/{session_id}, gmuxd proxies
// bidirectionally to the gmux-run Unix socket for that session.
//
// The proxy also manages terminal resize ownership: only one client at a time
// is the "resize owner" whose resize messages are forwarded to the runner.
// Other clients are passive and receive the owner's terminal size via SSE.
package wsproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"nhooyr.io/websocket"
)

// SessionStore provides terminal-size read/write for the proxy.
type SessionStore interface {
	SetTerminalSize(sessionID string, cols, rows uint16) bool
	GetTerminalSize(sessionID string) (cols, rows uint16, ok bool)
}

// conn tracks a single client WebSocket connection to a session.
type conn struct {
	id        string // opaque connection ID
	sessionID string
	ws        *websocket.Conn
}

// sessionConns tracks all connections to a session and which one owns resize.
type sessionConns struct {
	conns   []*conn
	ownerID string // connection ID of the current resize owner ("" = none)
}

// Proxy manages WebSocket proxying and resize ownership.
type Proxy struct {
	resolve SocketResolver
	sizer   SessionStore

	mu       sync.Mutex
	sessions map[string]*sessionConns // sessionID → connections
	nextID   uint64
}

// SocketResolver maps a session ID to a Unix socket path.
type SocketResolver func(sessionID string) (string, error)

// New creates a new WebSocket proxy.
func New(resolve SocketResolver, sizer SessionStore) *Proxy {
	return &Proxy{
		resolve:  resolve,
		sizer:    sizer,
		sessions: make(map[string]*sessionConns),
	}
}

// Handler returns the http.HandlerFunc for /ws/{sessionID}.
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("sessionID")
		if sessionID == "" {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}

		sockPath, err := p.resolve(sessionID)
		if err != nil {
			log.Printf("wsproxy: resolve %s: %v", sessionID, err)
			http.Error(w, fmt.Sprintf("session not found: %v", err), http.StatusNotFound)
			return
		}

		// Accept browser WebSocket.
		clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("wsproxy: accept: %v", err)
			return
		}

		// Connect to gmux-run's Unix socket.
		ctx := r.Context()
		backendConn, _, err := websocket.Dial(ctx, "ws://localhost/ws", &websocket.DialOptions{
			HTTPClient: &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return net.Dial("unix", sockPath)
					},
				},
			},
		})
		if err != nil {
			log.Printf("wsproxy: dial backend %s: %v", sockPath, err)
			clientConn.Close(websocket.StatusInternalError, "backend unavailable")
			return
		}

		// Register this connection.
		c := p.addConn(sessionID, clientConn)
		log.Printf("wsproxy: proxying %s conn=%s via %s", sessionID, c.id, sockPath)

		// Tell this client whether it's the resize owner.
		p.sendResizeState(ctx, c)

		// Read limits must exceed the scrollback buffer size (128KB)
		clientConn.SetReadLimit(256 * 1024)
		backendConn.SetReadLimit(256 * 1024)

		proxyCtx, proxyCancel := context.WithCancel(ctx)

		var wg sync.WaitGroup
		wg.Add(2)

		// Backend → Client (PTY output): pass through unchanged.
		go func() {
			defer wg.Done()
			defer proxyCancel()
			proxyMessages(proxyCtx, backendConn, clientConn)
		}()

		// Client → Backend (keyboard input + resize): intercept control messages.
		go func() {
			defer wg.Done()
			defer proxyCancel()
			p.proxyClientToBackend(proxyCtx, c, clientConn, backendConn)
		}()

		wg.Wait()

		clientConn.Close(websocket.StatusNormalClosure, "")
		backendConn.Close(websocket.StatusNormalClosure, "")

		// Unregister and maybe reassign resize owner.
		p.removeConn(c)
		log.Printf("wsproxy: session %s conn=%s disconnected", sessionID, c.id)
	}
}

// proxyClientToBackend reads messages from the client and either forwards
// them to the backend or handles them as control messages.
func (p *Proxy) proxyClientToBackend(ctx context.Context, c *conn, client, backend *websocket.Conn) {
	for {
		typ, data, err := client.Read(ctx)
		if err != nil {
			return
		}

		// Text messages might be JSON control messages (resize, claim_resize).
		if typ == websocket.MessageText {
			if p.handleControlMessage(ctx, c, data, backend) {
				continue // consumed — don't forward
			}
		}

		// Forward everything else to the backend.
		if err := backend.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// controlMsg is the minimal shape we peek at from client JSON messages.
type controlMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// handleControlMessage checks if a client message is a resize or claim_resize.
// Returns true if the message was consumed (should not be forwarded).
func (p *Proxy) handleControlMessage(ctx context.Context, c *conn, data []byte, backend *websocket.Conn) bool {
	var msg controlMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return false // not JSON — forward as-is
	}

	switch msg.Type {
	case "resize":
		p.mu.Lock()
		sc := p.sessions[c.sessionID]
		isOwner := sc != nil && sc.ownerID == c.id
		p.mu.Unlock()

		if !isOwner {
			return true // drop — only the owner can resize
		}

		// Forward to backend runner.
		if err := backend.Write(ctx, websocket.MessageText, data); err != nil {
			return true
		}

		// Update store with new terminal size → broadcasts via SSE.
		if msg.Cols > 0 && msg.Rows > 0 {
			p.sizer.SetTerminalSize(c.sessionID, msg.Cols, msg.Rows)
		}
		return true

	case "claim_resize":
		p.claimOwnership(ctx, c)
		return true

	default:
		return false // unknown type — forward to backend
	}
}

// addConn registers a new connection. First connection becomes resize owner.
func (p *Proxy) addConn(sessionID string, ws *websocket.Conn) *conn {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.nextID++
	c := &conn{
		id:        fmt.Sprintf("c%d", p.nextID),
		sessionID: sessionID,
		ws:        ws,
	}

	sc, ok := p.sessions[sessionID]
	if !ok {
		sc = &sessionConns{}
		p.sessions[sessionID] = sc
	}
	sc.conns = append(sc.conns, c)

	// First connection becomes resize owner automatically.
	if sc.ownerID == "" {
		sc.ownerID = c.id
	}

	return c
}

// removeConn unregisters a connection. If the owner disconnects,
// ownership passes to the next connected client.
func (p *Proxy) removeConn(c *conn) {
	p.mu.Lock()

	sc, ok := p.sessions[c.sessionID]
	if !ok {
		p.mu.Unlock()
		return
	}

	// Remove from list.
	for i, existing := range sc.conns {
		if existing.id == c.id {
			sc.conns = append(sc.conns[:i], sc.conns[i+1:]...)
			break
		}
	}

	// If no connections left, clean up.
	if len(sc.conns) == 0 {
		delete(p.sessions, c.sessionID)
		p.mu.Unlock()
		return
	}

	// If this was the owner, promote the next connection.
	var newOwner *conn
	if sc.ownerID == c.id {
		sc.ownerID = sc.conns[0].id
		newOwner = sc.conns[0]
	}
	p.mu.Unlock()

	// Notify the new owner outside the lock.
	if newOwner != nil {
		p.sendResizeState(context.Background(), newOwner)
	}
}

// claimOwnership makes a connection the resize owner for its session.
func (p *Proxy) claimOwnership(ctx context.Context, c *conn) {
	p.mu.Lock()
	sc, ok := p.sessions[c.sessionID]
	if !ok {
		p.mu.Unlock()
		return
	}

	wasOwner := sc.ownerID
	sc.ownerID = c.id

	// Collect all connections to notify.
	conns := make([]*conn, len(sc.conns))
	copy(conns, sc.conns)
	p.mu.Unlock()

	// Notify everyone: the new owner and former owner both need updated state.
	for _, cc := range conns {
		if cc.id == c.id || cc.id == wasOwner {
			p.sendResizeState(ctx, cc)
		}
	}
}

// sendResizeState sends a resize_state control message to a client telling
// it whether it's the resize owner, and the current terminal dimensions.
func (p *Proxy) sendResizeState(ctx context.Context, c *conn) {
	p.mu.Lock()
	sc := p.sessions[c.sessionID]
	if sc == nil {
		p.mu.Unlock()
		return
	}
	isOwner := sc.ownerID == c.id
	p.mu.Unlock()

	payload := map[string]any{
		"type":     "resize_state",
		"is_owner": isOwner,
	}
	// Include current terminal size so passive clients can resize immediately.
	if cols, rows, ok := p.sizer.GetTerminalSize(c.sessionID); ok && cols > 0 && rows > 0 {
		payload["cols"] = cols
		payload["rows"] = rows
	}

	msg, _ := json.Marshal(payload)
	// Best-effort — if the write fails the connection is closing anyway.
	_ = c.ws.Write(ctx, websocket.MessageText, msg)
}

// proxyMessages copies messages from src to dst until error or context cancel.
func proxyMessages(ctx context.Context, src, dst *websocket.Conn) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
