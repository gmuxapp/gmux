// Package ptyserver allocates a PTY, execs a command, and serves
// a WebSocket endpoint on a Unix socket. Replaces abduco + ttyd.
package ptyserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"nhooyr.io/websocket"
)

// maxScrollback is the number of lines kept in the virtual terminal's
// scrollback buffer. Lines older than this are discarded.
const maxScrollback = 2000

// ErrSocketInUse is returned by BindSocket when the requested socket
// path is already owned by a live listener (a probe at that path got
// a response). Callers that received this error from New should pick
// a different session id and retry. See ADR 0003 "Collision
// handling".
var ErrSocketInUse = errors.New("socket path already in use by a live runner")

// BindSocket creates and listens on a Unix socket at sockPath. It
// distinguishes a stale leftover socket file from a live owner:
//
//   - If a live owner answers a probe connection, returns
//     ErrSocketInUse without touching the file. The caller should
//     pick a different path.
//   - Otherwise, removes any stale file and listens.
//
// The TOCTOU window between the probe and the listen is harmless in
// practice: only gmux runners write to socketDir, and a runner that
// could win the race would itself be subject to this same probe on
// its next bind attempt.
func BindSocket(sockPath string) (net.Listener, error) {
	if probeSocket(sockPath) {
		return nil, ErrSocketInUse
	}
	_ = os.Remove(sockPath)
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)
	return net.Listen("unix", sockPath)
}

// probeSocket returns true if a Unix socket at sockPath accepts
// connections within a short timeout. Used to distinguish stale
// socket files from live runners.
func probeSocket(sockPath string) bool {
	if _, err := os.Stat(sockPath); err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 250*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func newScreen(cols, rows int, cursorCb func(visible bool)) *vt.Emulator {
	// Default to 80x24 when launched non-interactively (no terminal).
	// The first resize from a connecting client will set the real size.
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	e := vt.NewEmulator(cols, rows)
	e.SetScrollbackSize(maxScrollback)
	e.SetCallbacks(vt.Callbacks{
		CursorVisibility: cursorCb,
	})
	// The emulator writes responses (e.g. DSR cursor position reports)
	// to an internal pipe. If nothing reads them, Write blocks. We don't
	// need the responses, so drain them in the background.
	go io.Copy(io.Discard, e)
	return e
}

// renderScreen produces the ANSI snapshot: scrollback lines followed by
// the visible screen. The scrollback gives reconnecting clients context
// (previous output they can scroll up to). The visible screen is rendered
// row-by-row via CellAt (not Render()) because the emulator's internal
// buffer can grow beyond the declared height. Rows are joined with \r\n
// since bare \n wouldn't return the cursor to column 0.
func renderScreen(e *vt.Emulator) string {
	var sb strings.Builder

	// Scrollback: lines that scrolled off the top of the screen.
	if scrollback := e.Scrollback(); scrollback != nil {
		for _, line := range scrollback.Lines() {
			sb.WriteString(line.Render())
			sb.WriteString("\r\n")
		}
	}

	// Visible screen.
	w, h := e.Width(), e.Height()
	for y := 0; y < h; y++ {
		if y > 0 {
			sb.WriteString("\r\n")
		}
		line := make(uv.Line, w)
		for x := 0; x < w; x++ {
			if c := e.CellAt(x, y); c != nil {
				line[x] = *c
			}
		}
		sb.WriteString(line.Render())
	}
	return sb.String()
}

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
	screen       *vt.Emulator // virtual terminal for replay snapshots (guarded by mu)
	adapter      adapter.Adapter
	state        *session.State

	mu             sync.Mutex
	clients        map[*wsClient]struct{}
	localOut       io.Writer       // optional local terminal output sink
	scrollback     io.WriteCloser  // optional persistent scrollback sink (closed in waitChild)
	ptyCols        uint16          // last applied PTY cols (guarded by mu)
	ptyRows        uint16          // last applied PTY rows (guarded by mu)
	cursorHidden   bool            // tracks DECTCEM via callback (guarded by mu)
	screenPending  []byte          // raw PTY data not yet fed to screen (guarded by mu)
	lastClientLeft time.Time       // when the last WS client disconnected (guarded by mu)

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
	Command []string
	Cwd     string
	Env     []string
	// Listener is the pre-bound Unix socket the server serves
	// HTTP/WebSocket on. Required. Callers obtain one via
	// BindSocket so they can react to ErrSocketInUse (e.g.,
	// regenerate the session id) before any sessionID-dependent
	// setup runs. The server takes ownership: Close is called on
	// shutdown.
	Listener   net.Listener
	SocketPath string
	Cols       uint16
	Rows       uint16
	Adapter    adapter.Adapter
	State      *session.State
	// Version is reported to children via TERM_PROGRAM_VERSION.
	// Defaults to "dev" when empty.
	Version string
	// LocalOut, if non-nil, receives a copy of every PTY output chunk
	// from the moment the server starts reading. Set this at construction
	// time (rather than calling SetLocalOutput after New) when you need
	// to guarantee that fast-exiting commands can't race the wiring and
	// have their output dropped on the floor.
	LocalOut io.Writer
	// Scrollback, if non-nil, receives a copy of every PTY output
	// chunk for persistence to disk. Wired the same way as LocalOut
	// so fast-exiting commands can't lose output. The server takes
	// ownership: Close is called once after the final PTY drain in
	// waitChild, so callers must not Close it themselves and must
	// not pass a writer they need to keep using.
	Scrollback io.WriteCloser
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

	if cfg.Listener == nil {
		return nil, fmt.Errorf("ptyserver.New: cfg.Listener is required (use BindSocket)")
	}
	listener := cfg.Listener

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.Cwd
	cmd.Env = buildChildEnv(os.Environ(), cfg.Env, cfg.Version)

	// Start command in a new PTY
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cfg.Cols,
		Rows: cfg.Rows,
	})
	if err != nil {
		listener.Close()
		os.Remove(cfg.SocketPath)
		return nil, fmt.Errorf("start pty: %w", err)
	}

	s := &Server{
		cmd:        cmd,
		ptmx:       ptmx,
		sockPath:   cfg.SocketPath,
		listener:   listener,
		screen:     nil, // set below after s is constructed
		adapter:    cfg.Adapter,
		state:      cfg.State,
		clients:    make(map[*wsClient]struct{}),
		localOut:   cfg.LocalOut,   // wired before readPTY starts so early output is never lost
		scrollback: cfg.Scrollback, // same: wired pre-readPTY so fast-exit output is never lost
		ptyCols:    cfg.Cols,
		ptyRows:    cfg.Rows,
		done:       make(chan struct{}),
		ptyDone:    make(chan struct{}),
	}

	// The callback fires under s.mu (held during drainScreenLocked → screen.Write).
	s.screen = newScreen(int(cfg.Cols), int(cfg.Rows), func(visible bool) {
		s.cursorHidden = !visible
	})

	go s.readPTY()
	go s.waitChild()
	go s.processScreen()
	go s.serve()

	return s, nil
}

// drainScreenLocked feeds all pending raw PTY data to the virtual terminal
// emulator. This is the only place where screen.Write is called, ensuring the
// emulator stays off the hot path (readPTY flush). Caller must hold s.mu.
func (s *Server) drainScreenLocked() {
	if len(s.screenPending) == 0 {
		return
	}
	s.screen.Write(s.screenPending)
	s.screenPending = s.screenPending[:0]
}

// screenSyncInterval controls how often the background goroutine feeds
// pending PTY data to the virtual terminal emulator. Keeping this short
// bounds the amount of data that must be drained synchronously when a
// client connects (snapshot) or the scrollback text is requested.
const screenSyncInterval = 100 * time.Millisecond

// processScreen runs in a background goroutine, periodically draining
// screenPending into the vt.Emulator. This keeps the emulator roughly
// up-to-date without blocking the readPTY hot path.
func (s *Server) processScreen() {
	ticker := time.NewTicker(screenSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.drainScreenLocked()
			s.mu.Unlock()
		case <-s.ptyDone:
			// Final drain after PTY output is fully read.
			s.mu.Lock()
			s.drainScreenLocked()
			s.mu.Unlock()
			return
		}
	}
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
// Note: when Done closes, the PTY readout may still have buffered
// output that hasn't been flushed to LocalOut / WS clients yet. Wait
// on PTYDone() as well if you need to see the child's final bytes.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// PTYDone returns a channel that is closed after the PTY has been fully
// drained, meaning all output the child ever produced has been flushed
// through LocalOut and to every WS client. Always closes strictly after
// Done(). Callers that want to detach a local terminal without dropping
// the child's trailing output should wait on this before detaching.
func (s *Server) PTYDone() <-chan struct{} {
	return s.ptyDone
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
//
// For the initial wiring, prefer Config.LocalOut: calling this after
// New leaves a race window in which a fast-exiting child's output can
// be flushed before the writer is attached and be silently dropped.
// SetLocalOutput is the right tool for *changing* the sink mid-session
// (e.g. detaching when stdin closes), not for the first attach.
func (s *Server) SetLocalOutput(w io.Writer) {
	s.mu.Lock()
	detaching := s.localOut != nil && w == nil
	s.localOut = w
	noViewers := detaching && len(s.clients) == 0
	s.mu.Unlock()

	if noViewers {
		s.shrinkForReconnect()
	}
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
	s.screen.Close() // unblocks the DSR drain goroutine
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
	mux.HandleFunc("GET /scrollback/tail", s.handleScrollbackTail)
	mux.HandleFunc("POST /input", s.handleInput)
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
	s.drainScreenLocked()
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
// Caller must hold s.mu.
func (s *Server) screenText() string {
	return s.screen.String()
}

// handleScrollbackTail returns the last N lines of the session's
// scrollback plus the currently visible screen, as plain text.
// Intended for `gmux --tail N <id>`: a log-style peek at what's been
// happening inside a session.
//
// N is read from the ?n= query parameter; defaults to 50, capped at
// maxScrollback + the visible screen height. Trailing blank lines on
// the visible screen are trimmed so an idle TUI doesn't pad the output
// with empty rows.
func (s *Server) handleScrollbackTail(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}

	s.mu.Lock()
	s.drainScreenLocked()
	lines := s.scrollbackLinesLocked()
	s.mu.Unlock()

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, line := range lines {
		w.Write([]byte(line))
		w.Write([]byte("\n"))
	}
}

// scrollbackLinesLocked returns the scrollback history followed by the
// visible screen as plain-text lines (no ANSI). Trailing blank rows of
// the visible screen are trimmed. Caller must hold s.mu.
func (s *Server) scrollbackLinesLocked() []string {
	var lines []string

	if sb := s.screen.Scrollback(); sb != nil {
		for _, line := range sb.Lines() {
			lines = append(lines, plainLine(line))
		}
	}

	w, h := s.screen.Width(), s.screen.Height()
	screenLines := make([]string, h)
	for y := 0; y < h; y++ {
		row := make(uv.Line, w)
		for x := 0; x < w; x++ {
			if c := s.screen.CellAt(x, y); c != nil {
				row[x] = *c
			}
		}
		screenLines[y] = plainLine(row)
	}
	// Trim trailing empty rows — an idle TUI pads the screen with blanks.
	end := len(screenLines)
	for end > 0 && strings.TrimSpace(screenLines[end-1]) == "" {
		end--
	}
	lines = append(lines, screenLines[:end]...)
	return lines
}

// plainLine renders a terminal line as plain text (no ANSI styling),
// right-trimming trailing spaces so short lines don't emit padding.
func plainLine(line uv.Line) string {
	var sb strings.Builder
	for _, c := range line {
		if c.Content == "" {
			sb.WriteString(" ")
		} else {
			sb.WriteString(c.Content)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

// maxInputBytes caps the size of a single POST /input request body.
// The socket is owner-only, so this isn't a trust boundary — it just
// keeps a well-meaning `gmux --send` invocation from accidentally
// exhausting memory if someone pipes a huge file into it.
const maxInputBytes = 1 << 20 // 1 MiB

// handleInput writes the request body straight to the child PTY, as if
// the bytes had been typed at the terminal. Backs `gmux --send`.
//
// Access control is delegated to the Unix socket's file permissions
// (owner-only, 0o700): anyone who can connect() to this socket already
// owns the session and could do arbitrary worse things to it.
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInputBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.ptmx.Write(body); err != nil {
		http.Error(w, "write pty: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if s.cmd.Process == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// SIGHUP matches the "terminal closed" semantics of this endpoint:
	// interactive shells (bash, zsh) ignore SIGTERM but exit cleanly on
	// SIGHUP; TUI adapters treat SIGHUP the same as a graceful shutdown.
	// Sent to the process group so children (e.g. a subshell's commands)
	// receive it too.
	pid := s.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGHUP)
	log.Printf("ptyserver: sent SIGHUP to child pid %d", pid)

	// Block until the child actually exits (or escalate). Dismiss/restart
	// callers rely on this: once /kill returns, gmuxd immediately removes
	// the session and expects the runner's socket path to be free.
	// Returning early while a shell (e.g. fish) ignores SIGHUP causes
	// the next discovery scan to re-register the dead session.
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
		log.Printf("ptyserver: escalated to SIGKILL for child pid %d", pid)
		<-s.done
	}

	// Release the canonical socket path before responding. The runner
	// process will linger briefly for state.SetExited / deregister /
	// scrollback close, and its listener stays up on the inode for the
	// existing SSE/WS connections that need to drain (notably the
	// daemon's exit-event subscription). But the path is unreachable
	// to new dialers, so a daemon launching a replacement runner under
	// the same id (resume / restart, see ADR 0003) can BindSocket
	// without racing against this runner's shutdown sequence.
	//
	// Idempotent: a later os.Remove on the missing path is harmless;
	// any normal-exit code path that also tries to clean up the path
	// (Server.Shutdown's signal-handler call, or the kernel on
	// process exit) finds it already gone.
	if err := os.Remove(s.sockPath); err != nil && !os.IsNotExist(err) {
		log.Printf("ptyserver: kill: remove sockfile %s: %v", s.sockPath, err)
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
	// The screen state comes from a virtual terminal (charmbracelet/x/vt)
	// that processes every byte of PTY output. renderScreen serializes
	// the scrollback history followed by the visible screen as ANSI
	// sequences with style diffing.
	//
	// Sequence: BSU → reset → scrollback + screen → cursor → ESU
	s.mu.Lock()
	s.drainScreenLocked()
	snapshot := renderScreen(s.screen)
	cursorSeq := "\x1b[?25h" // show cursor (default)
	if s.cursorHidden {
		cursorSeq = "\x1b[?25l" // hide cursor
	}
	// Position cursor at the emulator's current location.
	pos := s.screen.CursorPosition()
	cursorPos := fmt.Sprintf("\x1b[%d;%dH", pos.Y+1, pos.X+1)
	bsu := "\x1b[?2026h"                     // Begin Synchronized Update
	resetSeq := "\x1b[r\x1b[H\x1b[2J\x1b[3J" // Reset scroll region + cursor home + erase display + erase scrollback
	esu := "\x1b[?2026l"                     // End Synchronized Update
	frame := []byte(bsu + resetSeq + snapshot + cursorPos + cursorSeq + esu)
	if err := client.write(websocket.MessageBinary, frame); err != nil {
		s.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		return
	}
	s.clients[client] = struct{}{}
	s.lastClientLeft = time.Time{} // reset: we have an active viewer
	s.mu.Unlock()

	// Client connected — they'll see the scrollback, so clear unread
	if s.state != nil {
		s.state.SetUnread(false)
	}

	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		noClients := len(s.clients) == 0 && s.localOut == nil
		if len(s.clients) == 0 {
			s.lastClientLeft = time.Now()
		}
		s.mu.Unlock()

		// When the last viewer disconnects, shrink the PTY by 1 column.
		// The next connecting client will send a resize with its real
		// viewport, which will differ from the shrunk size, naturally
		// triggering a SIGWINCH that forces the child TUI to do a full
		// redraw (including re-emitting kitty images). This avoids the
		// need for a visible wiggle on connect.
		if noClients {
			s.shrinkForReconnect()
		}
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

// shrinkForReconnect reduces the PTY width by 1 column so that the next
// connecting client's resize (which will carry the real viewport size)
// triggers a genuine dimension change. Most TUI frameworks only do a full
// re-render when width or height actually changes; without this, a client
// whose viewport matches the current PTY size would get a stale snapshot
// (missing kitty images, possible drift from the emulator's reconstruction).
//
// Called when the last viewer (WS client or local terminal) disconnects.
// The shrink happens while no one is watching, so there's no visible
// flicker. The child TUI redraws at cols-1, but nobody sees it.
//
// Safety: re-checks that no viewer has connected between the call-site
// check and the lock acquisition. Also skips if the child has exited
// (pointless to resize a dead process).
//
// State and resize broadcasts are intentionally skipped: the shrunk size
// is an internal detail, not a real terminal size change.
func (s *Server) shrinkForReconnect() {
	// Don't bother if the child has exited.
	select {
	case <-s.done:
		return
	default:
	}

	s.mu.Lock()
	if s.ptyCols <= 1 || s.ptyRows == 0 || len(s.clients) > 0 || s.localOut != nil {
		s.mu.Unlock()
		return
	}
	s.ptyCols--
	cols := s.ptyCols
	rows := s.ptyRows
	s.drainScreenLocked()
	s.screen.Resize(int(cols), int(rows))
	s.mu.Unlock()

	pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	if s.cmd.Process != nil {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGWINCH)
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
		// Drain pending data first so the emulator processes it at the
		// old size before switching to the new dimensions.
		s.drainScreenLocked()
		s.screen.Resize(int(msg.Cols), int(msg.Rows))
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

// activityGrace is the time after the last WS client disconnects before
// activity events are emitted. Suppresses false positives during session
// switching when the old session briefly has zero clients.
const activityGrace = 500 * time.Millisecond

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
			if ev := s.adapter.Monitor(data); ev != nil {
				if ev.Title != "" {
					s.state.SetAdapterTitle(ev.Title)
				}
				if ev.Status != nil {
					s.state.SetStatus(ev.Status)
				}
			}
		}

		// Queue data for the virtual terminal emulator (processed by
		// processScreen in the background). Snapshot the client list
		// atomically so new clients always see their replay frame first.
		s.mu.Lock()
		s.screenPending = append(s.screenPending, data...)
		localOut := s.localOut
		clients := make([]*wsClient, 0, len(s.clients))
		for c := range s.clients {
			clients = append(clients, c)
		}
		hasRemoteClients := len(clients) > 0
		lastLeft := s.lastClientLeft
		s.mu.Unlock()

		// Emit activity only when no client is viewing and the grace
		// period has elapsed. The grace period suppresses false positives
		// during session switching (brief disconnect window).
		if !hasRemoteClients && s.state != nil {
			if lastLeft.IsZero() || time.Since(lastLeft) > activityGrace {
				s.state.EmitActivity()
			}
		}

		if localOut != nil {
			localOut.Write(data)
		}
		if s.scrollback != nil {
			// Best-effort: scrollback Write contract is no-error,
			// IO failures are sticky and surfaced via Close.
			s.scrollback.Write(data)
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

	// Now that the final flush has run, close the persistent
	// scrollback sink. Any IO error from the lifetime of the
	// writer surfaces here — we log but don't fail, since the
	// child has already exited and the scrollback is best-effort.
	if s.scrollback != nil {
		if err := s.scrollback.Close(); err != nil {
			log.Printf("ptyserver: scrollback close: %v", err)
		}
		s.scrollback = nil
	}

	s.mu.Lock()
	for c := range s.clients {
		c.conn.Close(websocket.StatusNormalClosure, "process exited")
	}
	s.mu.Unlock()
}

// buildChildEnv composes the environment passed to PTY children.
//
// Layering, in order:
//  1. parent (typically os.Environ()) — inherits the daemon/user env;
//  2. caller-supplied extras (cfg.Env from the adapter / runner);
//  3. terminal capability advertisements that always win, because the
//     frontend's actual capabilities don't depend on what the parent
//     thinks: TERM_PROGRAM=gmux, TERM_PROGRAM_VERSION=<version>,
//     COLORTERM=truecolor, KITTY_WINDOW_ID=1 (xterm.js + image addon
//     handles kitty graphics, sixel, and iTerm2 images);
//  4. TERM=xterm-256color, but only if no earlier layer provided one.
//     When gmuxd is launched from a non-interactive context (systemd
//     unit, browser-launched shell inheriting the daemon's env) TERM
//     may be missing, which makes curses programs like lazygit abort
//     with "terminal entry not found: term not set". Defaulting matches
//     what the xterm.js frontend actually renders.
//
// version falls back to "dev" when empty so TERM_PROGRAM_VERSION is
// never a bare "=".
func buildChildEnv(parent, extra []string, version string) []string {
	if version == "" {
		version = "dev"
	}
	env := make([]string, 0, len(parent)+len(extra)+5)
	for _, e := range parent {
		// GMUX_RESUME_ID is a private daemon→runner directive (see
		// ADR 0003). Inheriting it into PTY children would let a
		// nested `gmux foo` invocation inside a session try to
		// re-bind the parent runner's id; it'd survive on the
		// collision fallback, but that's exactly the safety-net
		// dependency the dedicated env var name was supposed to
		// avoid. Strip on the way out.
		if strings.HasPrefix(e, "GMUX_RESUME_ID=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, extra...)
	env = append(env,
		"TERM_PROGRAM=gmux",
		"TERM_PROGRAM_VERSION="+version,
		"COLORTERM=truecolor",
		"KITTY_WINDOW_ID=1",
	)
	if !hasEnv(env, "TERM") {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// hasEnv reports whether env contains a NAME=... entry for the given name.
func hasEnv(env []string, name string) bool {
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
