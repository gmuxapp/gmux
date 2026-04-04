// Package wsproxy provides a WebSocket reverse proxy from gmuxd to gmux-run
// session sockets. Browser connects to gmuxd /ws/{session_id}, gmuxd proxies
// bidirectionally to the gmux-run Unix socket for that session.
//
// The proxy is a transparent pipe: resize messages from any client are
// forwarded to the runner, and terminal_resize events from the runner are
// forwarded to all clients. The browser decides locally whether to drive
// resize or follow (pill). No ownership tracking lives here.
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

// Proxy manages WebSocket proxying between browsers and runners.
type Proxy struct {
	resolve SocketResolver
	sizer   SessionStore

	mu       sync.Mutex
	sessions map[string][]*websocket.Conn // sessionID → browser connections
}

// SocketResolver maps a session ID to a Unix socket path.
type SocketResolver func(sessionID string) (string, error)

// New creates a new WebSocket proxy.
func New(resolve SocketResolver, sizer SessionStore) *Proxy {
	return &Proxy{
		resolve:  resolve,
		sizer:    sizer,
		sessions: make(map[string][]*websocket.Conn),
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

		p.addConn(sessionID, clientConn)
		log.Printf("wsproxy: proxying %s via %s", sessionID, sockPath)

		// Read limits must exceed the scrollback buffer size (128KB)
		// Client sends keyboard input + resize messages (small).
		clientConn.SetReadLimit(256 * 1024)
		// Backend sends terminal snapshots on connect which can be large
		// (full screen state with ANSI attributes, e.g. 174x66 = ~1.3MB).
		backendConn.SetReadLimit(4 * 1024 * 1024)

		proxyCtx, proxyCancel := context.WithCancel(ctx)

		var wg sync.WaitGroup
		wg.Add(2)

		// Backend → Client (PTY output + terminal_resize events).
		go func() {
			defer wg.Done()
			defer proxyCancel()
			p.proxyBackendToClient(proxyCtx, sessionID, backendConn, clientConn)
		}()

		// Client → Backend (keyboard input + resize).
		go func() {
			defer wg.Done()
			defer proxyCancel()
			p.proxyClientToBackend(proxyCtx, clientConn, backendConn)
		}()

		wg.Wait()

		clientConn.Close(websocket.StatusNormalClosure, "")
		backendConn.Close(websocket.StatusNormalClosure, "")

		p.removeConn(sessionID, clientConn)
		log.Printf("wsproxy: session %s disconnected", sessionID)
	}
}

// proxyClientToBackend forwards all client messages to the backend.
// Resize messages are tagged with source=web_client before forwarding.
func (p *Proxy) proxyClientToBackend(ctx context.Context, client, backend *websocket.Conn) {
	for {
		typ, data, err := client.Read(ctx)
		if err != nil {
			return
		}

		// Tag resize messages with source so the runner can include it in
		// terminal_resize broadcasts.
		if typ == websocket.MessageText {
			var peek struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &peek) == nil && peek.Type == "resize" {
				if augmented, err := injectField(data, "source", "web_client"); err == nil {
					data = augmented
				}
			}
		}

		if err := backend.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// proxyBackendToClient forwards backend messages to the browser and intercepts
// terminal_resize events to update the store's terminal dimensions.
func (p *Proxy) proxyBackendToClient(ctx context.Context, sessionID string, src, dst *websocket.Conn) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return
		}

		if typ == websocket.MessageText {
			var msg struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "terminal_resize" {
				if msg.Cols > 0 && msg.Rows > 0 {
					p.sizer.SetTerminalSize(sessionID, msg.Cols, msg.Rows)
				}
			}
		}

		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

func (p *Proxy) addConn(sessionID string, ws *websocket.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[sessionID] = append(p.sessions[sessionID], ws)
}

func (p *Proxy) removeConn(sessionID string, ws *websocket.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	conns := p.sessions[sessionID]
	for i, c := range conns {
		if c == ws {
			p.sessions[sessionID] = append(conns[:i], conns[i+1:]...)
			break
		}
	}
	if len(p.sessions[sessionID]) == 0 {
		delete(p.sessions, sessionID)
	}
}

// injectField adds or overwrites a string field in a JSON object.
func injectField(data []byte, key, value string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	v, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m[key] = v
	return json.Marshal(m)
}
