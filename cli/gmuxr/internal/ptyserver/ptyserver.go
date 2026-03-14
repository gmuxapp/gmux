// Package ptyserver allocates a PTY, execs a command, and serves
// a WebSocket endpoint on a Unix socket. Replaces abduco + ttyd.
package ptyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/ringbuf"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/session"
	"nhooyr.io/websocket"
)

const defaultScrollbackSize = 128 * 1024 // 128KB

// ResizeMsg is the JSON message clients send to resize the terminal.
type ResizeMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// Server holds a PTY and serves WebSocket connections.
type Server struct {
	cmd        *exec.Cmd
	ptmx       *os.File
	sockPath   string
	listener   net.Listener
	scrollback *ringbuf.RingBuf
	adapter    adapter.Adapter
	state      *session.State

	mu      sync.Mutex
	clients map[*wsClient]struct{}

	done chan struct{} // closed when child exits
	err  error        // child exit error
}

type wsClient struct {
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	readonly bool
}

// Config for creating a new PTY server.
type Config struct {
	Command        []string
	Cwd            string
	Env            []string
	SocketPath     string
	Cols           uint16
	Rows           uint16
	ScrollbackSize int // bytes, 0 = default (128KB)
	Adapter        adapter.Adapter
	State          *session.State
}

// New creates and starts a PTY server.
func New(cfg Config) (*Server, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("no command specified")
	}
	if cfg.Cols == 0 {
		cfg.Cols = 80
	}
	if cfg.Rows == 0 {
		cfg.Rows = 25
	}
	scrollbackSize := cfg.ScrollbackSize
	if scrollbackSize <= 0 {
		scrollbackSize = defaultScrollbackSize
	}

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.Cwd
	cmd.Env = append(os.Environ(), cfg.Env...)

	// Start command in a new PTY
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cfg.Cols,
		Rows: cfg.Rows,
	})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	// Ensure socket dir exists and remove stale socket
	os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o755)
	os.Remove(cfg.SocketPath)

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		ptmx.Close()
		cmd.Process.Kill()
		return nil, fmt.Errorf("listen unix: %w", err)
	}

	s := &Server{
		cmd:        cmd,
		ptmx:       ptmx,
		sockPath:   cfg.SocketPath,
		listener:   listener,
		scrollback: ringbuf.New(scrollbackSize),
		adapter:    cfg.Adapter,
		state:      cfg.State,
		clients:    make(map[*wsClient]struct{}),
		done:       make(chan struct{}),
	}

	go s.readPTY()
	go s.waitChild()
	go s.serve()

	return s, nil
}

// Pid returns the child process PID.
func (s *Server) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// SocketPath returns the Unix socket path.
func (s *Server) SocketPath() string {
	return s.sockPath
}

// Done returns a channel that is closed when the child process exits.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// ExitCode returns the child process exit code (only valid after Done).
func (s *Server) ExitCode() int {
	if s.err == nil {
		return 0
	}
	if exitErr, ok := s.err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

// Shutdown closes the listener and all connections.
func (s *Server) Shutdown() {
	s.listener.Close()
	s.ptmx.Close()
	os.Remove(s.sockPath)

	s.mu.Lock()
	for c := range s.clients {
		c.cancel()
	}
	s.mu.Unlock()
}

func (s *Server) serve() {
	mux := http.NewServeMux()

	// HTTP endpoints (checked first via explicit paths)
	mux.HandleFunc("GET /meta", s.handleMeta)
	mux.HandleFunc("PUT /status", s.handlePutStatus)
	mux.HandleFunc("PATCH /meta", s.handlePatchMeta)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /kill", s.handleKill)

	// WebSocket terminal attach (fallback for / with Upgrade header)
	mux.HandleFunc("/", s.handleWS)

	server := &http.Server{Handler: mux}
	server.Serve(s.listener)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	data, err := s.state.JSON()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *Server) handlePutStatus(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// "null" clears the status
	if string(body) == "null" {
		s.state.SetStatus(nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var status adapter.Status
	if err := json.Unmarshal(body, &status); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate state
	switch status.State {
	case "active", "attention", "success", "error", "paused", "info":
		// ok
	default:
		http.Error(w, "invalid state: must be active|attention|success|error|paused|info", http.StatusBadRequest)
		return
	}

	s.state.SetStatus(&status)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePatchMeta(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var patch struct {
		Title    *string `json:"title"`
		Subtitle *string `json:"subtitle"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.state.PatchMeta(patch.Title, patch.Subtitle)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	if s.cmd.Process != nil {
		// Send SIGTERM to the child process group for clean shutdown
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
		log.Printf("ptyserver: sent SIGTERM to child pid %d", s.cmd.Process.Pid)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := s.state.Subscribe()
	defer s.state.Unsubscribe(ch)

	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(evt.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // local Unix socket, no origin check needed
	})
	if err != nil {
		log.Printf("ptyserver: ws accept: %v", err)
		return
	}
	conn.SetReadLimit(64 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	client := &wsClient{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
	}

	// Replay scrollback before adding to live clients.
	// This is done under the lock to ensure no output is missed between
	// snapshot and registration — the client sees the full history plus
	// all subsequent live data with no gap.
	s.mu.Lock()
	snapshot := s.scrollback.Snapshot()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	if len(snapshot) > 0 {
		// Wrap scrollback replay in synchronized output (DEC 2026) so xterm
		// renders the entire replay as one atomic frame — no flicker.
		//
		// Inside the sync block we include terminal reset sequences so xterm
		// clears its internal state before writing the scrollback. This means
		// the frontend never needs to call clear()/reset() — the old content
		// is replaced atomically when xterm processes ESU.
		//
		// Sequence: BSU → reset(scroll region, cursor home, erase all) → scrollback → ESU
		bsu := []byte("\x1b[?2026h")                      // Begin Synchronized Update
		resetSeq := []byte("\x1b[r\x1b[H\x1b[2J\x1b[3J") // Reset scroll region + cursor home + erase display + erase scrollback
		esu := []byte("\x1b[?2026l")                       // End Synchronized Update
		frame := make([]byte, 0, len(bsu)+len(resetSeq)+len(snapshot)+len(esu))
		frame = append(frame, bsu...)
		frame = append(frame, resetSeq...)
		frame = append(frame, snapshot...)
		frame = append(frame, esu...)
		if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			s.mu.Lock()
			delete(s.clients, client)
			s.mu.Unlock()
			conn.Close(websocket.StatusNormalClosure, "")
			cancel()
			return
		}
	}

	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
	}()

	// Read from WebSocket, write to PTY
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return // client disconnected
		}

		// Text frames might be resize messages
		if typ == websocket.MessageText {
			var msg ResizeMsg
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
				s.resize(msg.Cols, msg.Rows)
				continue
			}
		}

		// Write input to PTY
		if _, err := s.ptmx.Write(data); err != nil {
			return
		}
	}
}

func (s *Server) resize(cols, rows uint16) {
	if cols == 0 || rows == 0 {
		return
	}
	pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})

	// Send SIGWINCH to the child process group
	if s.cmd.Process != nil {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGWINCH)
	}
}

func (s *Server) readPTY() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			data := buf[:n]

			// Feed adapter monitor (cheap, no alloc expected)
			if s.adapter != nil {
				if status := s.adapter.Monitor(data); status != nil {
					s.state.SetStatus(status)
				}
			}

			// Store in scrollback (under lock with broadcast to avoid gaps)
			s.mu.Lock()
			s.scrollback.Write(data)
			clients := make([]*wsClient, 0, len(s.clients))
			for c := range s.clients {
				clients = append(clients, c)
			}
			s.mu.Unlock()

			for _, c := range clients {
				if err := c.conn.Write(c.ctx, websocket.MessageBinary, data); err != nil {
					c.cancel()
				}
			}
		}
		if err != nil {
			return // PTY closed
		}
	}
}

func (s *Server) waitChild() {
	s.err = s.cmd.Wait()
	close(s.done)

	s.mu.Lock()
	for c := range s.clients {
		c.conn.Close(websocket.StatusNormalClosure, "process exited")
	}
	s.mu.Unlock()
}
