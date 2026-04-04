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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/vito/midterm"
	"nhooyr.io/websocket"
)

// ResizeMsg is the JSON message clients send to resize the terminal.
type ResizeMsg struct {
	Type        string `json:"type"`
	Cols        uint16 `json:"cols"`
	Rows        uint16 `json:"rows"`
	PixelWidth  uint16 `json:"pixelWidth,omitempty"`
	PixelHeight uint16 `json:"pixelHeight,omitempty"`
	// Source identifies who triggered the resize: "local_tty" or "web_client".
	Source string `json:"source,omitempty"`
}

// Server holds a PTY and serves WebSocket connections.
type Server struct {
	cmd      *exec.Cmd
	ptmx     *os.File
	sockPath string
	listener net.Listener
	screen   *midterm.Terminal // virtual terminal for replay snapshots (guarded by mu)
	adapter  adapter.Adapter
	state    *session.State

	mu       sync.Mutex
	clients  map[*wsClient]struct{}
	localOut io.Writer // optional local terminal output sink
	ptyCols  uint16    // last applied PTY cols (guarded by mu)
	ptyRows  uint16    // last applied PTY rows (guarded by mu)

	done    chan struct{} // closed when child exits
	ptyDone chan struct{} // closed when readPTY finishes draining
	err     error         // child exit error
}

type wsClient struct {
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	readonly bool
	writeMu  sync.Mutex
}

func (c *wsClient) write(typ websocket.MessageType, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(c.ctx, typ, data)
}

// Config for creating a new PTY server.
type Config struct {
	Command    []string
	Cwd        string
	Env        []string
	SocketPath string
	Cols       uint16
	Rows       uint16
	Adapter    adapter.Adapter
	State      *session.State
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

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.Cwd
	cmd.Env = append(os.Environ(), cfg.Env...)
	// Advertise terminal capabilities to child processes.
	// Our frontend (xterm.js + image addon) supports kitty graphics, sixel, and iTerm2 images.
	// Set KITTY_WINDOW_ID so programs that check for kitty graphics support (e.g. pi, viu)
	// will use it. This is legitimate — our terminal genuinely handles the kitty protocol.
	cmd.Env = append(cmd.Env,
		"TERM_PROGRAM=gmux",
		"TERM_PROGRAM_VERSION=0.1.0",
		"COLORTERM=truecolor",
		"KITTY_WINDOW_ID=1",
	)

	// Start command in a new PTY
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cfg.Cols,
		Rows: cfg.Rows,
	})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	// Ensure socket dir exists (owner-only) and remove stale socket.
	os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o700)
	os.Remove(cfg.SocketPath)

	// Set umask so the socket file itself is owner-only (0700).
	oldUmask := syscall.Umask(0o077)
	listener, err := net.Listen("unix", cfg.SocketPath)
	syscall.Umask(oldUmask)
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
		screen:     midterm.NewTerminal(int(cfg.Rows), int(cfg.Cols)),
		adapter:    cfg.Adapter,
		state:      cfg.State,
		clients:    make(map[*wsClient]struct{}),
		ptyCols:    cfg.Cols,
		ptyRows:    cfg.Rows,
		done:       make(chan struct{}),
		ptyDone:    make(chan struct{}),
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

// SetLocalOutput sets a writer that receives a copy of all PTY output.
// Used for transparent local terminal attach. Pass nil to detach.
func (s *Server) SetLocalOutput(w io.Writer) {
	s.mu.Lock()
	s.localOut = w
	s.mu.Unlock()
}

// WritePTY writes raw bytes to the PTY input (as if typed by the user).
func (s *Server) WritePTY(p []byte) (int, error) {
	return s.ptmx.Write(p)
}

// Resize changes the PTY window size and signals the child.
// Called by the local terminal (localterm) on SIGWINCH — always tagged as local_tty.
func (s *Server) Resize(cols, rows uint16) {
	s.resize(ResizeMsg{Cols: cols, Rows: rows, Source: "local_tty"})
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
	mux.HandleFunc("GET /scrollback/text", s.handleScrollbackText)
	mux.HandleFunc("PUT /status", s.handlePutStatus)
	mux.HandleFunc("PUT /slug", s.handlePutSlug)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /kill", s.handleKill)

	// WebSocket terminal attach (fallback for / with Upgrade header)
	mux.HandleFunc("/", s.handleWS)

	server := &http.Server{Handler: mux}
	server.Serve(s.listener)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	data, err := s.state.MarshalJSON()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleScrollbackText returns the visible screen content as plain text,
// suitable for content-similarity matching (ADR-0009).
func (s *Server) handleScrollbackText(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	text := s.screenText()
	s.mu.Unlock()

	// Return only the tail, 2000 chars is plenty for similarity matching.
	const maxChars = 2000
	if len(text) > maxChars {
		text = text[len(text)-maxChars:]
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(text))
}

// screenText returns the visible screen content as plain text.
// Leading and trailing blank rows are omitted. Blank rows between
// content are preserved.
// Caller must hold s.mu.
func (s *Server) screenText() string {
	// Find the last non-empty row to avoid trailing blank lines.
	lastRow := -1
	for row := s.screen.Height - 1; row >= 0; row-- {
		if strings.TrimRight(string(s.screen.Content[row]), " ") != "" {
			lastRow = row
			break
		}
	}
	if lastRow < 0 {
		return ""
	}

	var sb strings.Builder
	for row := 0; row <= lastRow; row++ {
		line := strings.TrimRight(string(s.screen.Content[row]), " ")
		if line != "" || sb.Len() > 0 {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(line)
		}
	}
	return sb.String()
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

	s.state.SetStatus(&status)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePutSlug(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	slug := string(body)
	if slug == "" {
		http.Error(w, "slug is empty", http.StatusBadRequest)
		return
	}
	s.state.SetSlug(slug)
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

	// Replay screen state, then register for live data.
	// All steps happen under s.mu so readPTY cannot send live data to
	// this client before the snapshot frame.
	//
	// Ordering guarantee: snapshot is always the first message the client
	// receives, followed by any live data from subsequent readPTY cycles.
	//
	// The screen state comes from a virtual terminal (midterm) that
	// processes every byte of PTY output. MarshalBinary serializes the
	// full screen (content, colors, cursor position/visibility, scroll
	// region) as standard ANSI sequences with absolute positioning.
	//
	// Sequence: BSU → reset → screen state → ESU
	s.mu.Lock()
	snapshot, marshalErr := s.screen.MarshalBinary()
	if marshalErr != nil {
		log.Printf("ptyserver: marshal screen: %v", marshalErr)
		snapshot = nil
	}
	bsu := []byte("\x1b[?2026h")                     // Begin Synchronized Update
	resetSeq := []byte("\x1b[r\x1b[H\x1b[2J\x1b[3J") // Reset scroll region + cursor home + erase display + erase scrollback
	esu := []byte("\x1b[?2026l")                     // End Synchronized Update
	frame := make([]byte, 0, len(bsu)+len(resetSeq)+len(snapshot)+len(esu))
	frame = append(frame, bsu...)
	frame = append(frame, resetSeq...)
	frame = append(frame, snapshot...)
	frame = append(frame, esu...)
	if err := client.write(websocket.MessageBinary, frame); err != nil {
		s.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		return
	}
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	// Client connected — they'll see the scrollback, so clear unread
	if s.state != nil {
		s.state.SetUnread(false)
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
				s.resize(msg)
				continue
			}
		}

		// Write input to PTY
		if _, err := s.ptmx.Write(data); err != nil {
			return
		}
	}
}

func (s *Server) resize(msg ResizeMsg) {
	if msg.Cols == 0 || msg.Rows == 0 {
		return
	}

	// Check if the PTY size actually changed. Skipping redundant SIGWINCH
	// prevents TUI apps from redrawing their entire screen unnecessarily,
	// which is the main source of "rewrite the entire log" slowness on
	// reconnect or duplicate resize events.
	s.mu.Lock()
	sizeChanged := msg.Cols != s.ptyCols || msg.Rows != s.ptyRows
	if sizeChanged {
		s.ptyCols = msg.Cols
		s.ptyRows = msg.Rows
		s.screen.Resize(int(msg.Rows), int(msg.Cols))
	}
	s.mu.Unlock()

	if sizeChanged {
		pty.Setsize(s.ptmx, &pty.Winsize{
			Cols: msg.Cols,
			Rows: msg.Rows,
			X:    msg.PixelWidth,
			Y:    msg.PixelHeight,
		})

		// Send SIGWINCH to the child process group.
		if s.cmd.Process != nil {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGWINCH)
		}
	}

	// Always update state and broadcast so all clients stay in sync,
	// even if the PTY size didn't change (idempotent metadata update).
	if s.state != nil {
		s.state.SetTerminalSize(msg.Cols, msg.Rows)
	}

	// Broadcast terminal_resize to all connected WS clients so every browser
	// can update its xterm size and the proxy can update ownership/store.
	payload, err := json.Marshal(map[string]any{
		"type":   "terminal_resize",
		"cols":   msg.Cols,
		"rows":   msg.Rows,
		"source": msg.Source,
	})
	if err != nil {
		return
	}

	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		if err := c.write(websocket.MessageText, payload); err != nil {
			c.cancel()
		}
	}
}

// coalesceMaxBytes is the maximum accumulated data before forcing a flush,
// even if the coalesce timer hasn't fired yet. Keeps latency bounded.
const coalesceMaxBytes = 32 * 1024

// coalesceInterval is how long readPTY waits for more data before flushing.
// Chosen to be below one 60 fps frame (~16 ms) so the browser can still
// render at full frame rate while dramatically reducing WS message count
// during bursts (e.g. TUI redraws after SIGWINCH).
const coalesceInterval = 8 * time.Millisecond

func (s *Server) readPTY() {
	defer close(s.ptyDone)

	buf := make([]byte, 32*1024)
	var accum []byte
	timer := time.NewTimer(coalesceInterval)
	timer.Stop()

	flush := func() {
		if len(accum) == 0 {
			return
		}
		data := accum
		accum = nil

		// Process adapter/title hooks on the accumulated chunk.
		if title := adapters.ParseOSCTitle(data); title != "" {
			s.state.SetShellTitle(title)
		}
		if s.adapter != nil {
			if status := s.adapter.Monitor(data); status != nil {
				if status.Title != "" {
					s.state.SetAdapterTitle(status.Title)
					status.Title = ""
				}
				s.state.SetStatus(status)
			}
		}

		// Feed the virtual terminal and snapshot client list atomically.
		s.mu.Lock()
		s.screen.Write(data)
		localOut := s.localOut
		clients := make([]*wsClient, 0, len(s.clients))
		for c := range s.clients {
			clients = append(clients, c)
		}
		hasRemoteClients := len(clients) > 0
		s.mu.Unlock()

		if !hasRemoteClients && s.state != nil {
			s.state.EmitActivity()
		}

		if localOut != nil {
			localOut.Write(data)
		}

		for _, c := range clients {
			if err := c.write(websocket.MessageBinary, data); err != nil {
				c.cancel()
			}
		}
	}

	readCh := make(chan []byte, 4)
	readDone := make(chan error, 1)

	// Separate goroutine for blocking PTY reads so we can select on
	// both incoming data and the coalesce timer.
	go func() {
		for {
			n, err := s.ptmx.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				readCh <- chunk
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	}()

	for {
		select {
		case chunk := <-readCh:
			accum = append(accum, chunk...)
			if len(accum) >= coalesceMaxBytes {
				timer.Stop()
				flush()
			} else {
				// Reset the coalesce timer. On the first chunk this
				// starts the window; on subsequent chunks it extends it.
				timer.Reset(coalesceInterval)
			}

		case <-timer.C:
			flush()

		case <-readDone:
			timer.Stop()
			// Drain any remaining chunks that were queued before the
			// reader goroutine signaled completion.
		drain:
			for {
				select {
				case chunk := <-readCh:
					accum = append(accum, chunk...)
				default:
					break drain
				}
			}
			flush()
			return
		}
	}
}


func (s *Server) waitChild() {
	s.err = s.cmd.Wait()
	close(s.done)

	// Wait for readPTY to finish draining all buffered PTY output before
	// closing client connections. Without this, the coalesce buffer may
	// still hold the child's final output when we close the WebSocket,
	// causing data loss.
	<-s.ptyDone

	s.mu.Lock()
	for c := range s.clients {
		c.conn.Close(websocket.StatusNormalClosure, "process exited")
	}
	s.mu.Unlock()
}
