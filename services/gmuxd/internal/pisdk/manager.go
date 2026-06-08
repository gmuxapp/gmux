// Package pisdk manages pi-sdk subprocess sessions in gmuxd.
// Each session owns one Node subprocess that communicates via JSON lines
// on stdin/stdout. This package handles subprocess lifecycle, WebSocket
// fan-out, and store updates.
package pisdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"nhooyr.io/websocket"
)

// Manager manages pi-sdk subprocess sessions.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*subprocess
	store    *store.Store
}

type subprocess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}

	mu    sync.Mutex
	conns []*websocket.Conn
}

// New creates a Manager backed by the given session store.
func New(s *store.Store) *Manager {
	return &Manager{
		sessions: make(map[string]*subprocess),
		store:    s,
	}
}

// Launch spawns the Node subprocess for a session that is already registered
// in the store. nodeCmd is the full argv (e.g. ["node", "/path/to/index.js", "--cwd", cwd]).
func (m *Manager) Launch(sessionID string, nodeCmd []string) error {
	if len(nodeCmd) == 0 {
		return fmt.Errorf("pisdk: empty command")
	}

	cmd := exec.Command(nodeCmd[0], nodeCmd[1:]...)
	cmd.Stderr = os.Stderr // surface Node errors in the gmuxd log

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("pisdk: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pisdk: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pisdk: start: %w", err)
	}

	proc := &subprocess{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[sessionID] = proc
	m.mu.Unlock()

	// Update store with PID.
	m.store.Update(sessionID, func(s *store.Session) {
		s.Pid = cmd.Process.Pid
	})

	go m.readLoop(sessionID, proc, stdout)
	go m.waitLoop(sessionID, proc)

	log.Printf("pisdk: launched session %s pid=%d", sessionID, cmd.Process.Pid)
	return nil
}

// readLoop reads JSON-line events from stdout and broadcasts them to all
// connected WebSocket clients. Special events (session_ready) also update
// the store.
func (m *Manager) readLoop(sessionID string, proc *subprocess, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		// Peek at the event type.
		var peek struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(line, &peek); err != nil {
			log.Printf("pisdk: %s: bad JSON from subprocess: %v", sessionID, err)
			continue
		}

		// session_ready carries the resolved model name — show it as subtitle.
		if peek.Type == "session_ready" && peek.Model != "" {
			m.store.Update(sessionID, func(s *store.Session) {
				s.Subtitle = peek.Model
			})
		}

		// Broadcast to all connected clients.
		msg := append([]byte(nil), line...) // copy before concurrent reads
		proc.mu.Lock()
		conns := append([]*websocket.Conn(nil), proc.conns...)
		proc.mu.Unlock()

		for _, conn := range conns {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
				log.Printf("pisdk: %s: write to client: %v", sessionID, err)
			}
			cancel()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("pisdk: %s: stdout read error: %v", sessionID, err)
	}
}

// waitLoop waits for the subprocess to exit, then marks the session dead in
// the store and closes all open WebSocket connections.
func (m *Manager) waitLoop(sessionID string, proc *subprocess) {
	_ = proc.cmd.Wait()
	close(proc.done)

	exitCode := 0
	if proc.cmd.ProcessState != nil {
		exitCode = proc.cmd.ProcessState.ExitCode()
	}

	now := time.Now().UTC().Format(time.RFC3339)
	code := exitCode // capture for closure
	m.store.Update(sessionID, func(s *store.Session) {
		s.Alive = false
		s.ExitCode = &code
		s.ExitedAt = now
	})

	// Close all open WebSocket connections.
	proc.mu.Lock()
	conns := proc.conns
	proc.conns = nil
	proc.mu.Unlock()

	for _, conn := range conns {
		conn.Close(websocket.StatusNormalClosure, "process exited")
	}

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	log.Printf("pisdk: session %s exited (code %d)", sessionID, exitCode)
}

// HandleWebSocket accepts a WebSocket upgrade for a pi-sdk session, adds the
// connection to the broadcast fan-out, and relays incoming client messages to
// the subprocess stdin as JSON lines.
func (m *Manager) HandleWebSocket(w http.ResponseWriter, r *http.Request, sessionID string) {
	m.mu.Lock()
	proc := m.sessions[sessionID]
	m.mu.Unlock()

	if proc == nil {
		http.Error(w, "session not running", http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("pisdk: ws accept %s: %v", sessionID, err)
		return
	}

	// Register this connection for broadcast.
	proc.mu.Lock()
	proc.conns = append(proc.conns, conn)
	proc.mu.Unlock()

	defer func() {
		proc.mu.Lock()
		for i, c := range proc.conns {
			if c == conn {
				proc.conns = append(proc.conns[:i], proc.conns[i+1:]...)
				break
			}
		}
		proc.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Relay: client messages → subprocess stdin.
	ctx := r.Context()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		line := append(data, '\n')
		if _, err := proc.stdin.Write(line); err != nil {
			log.Printf("pisdk: %s: stdin write: %v", sessionID, err)
			return
		}
	}
}

// IsRunning reports whether the subprocess for sessionID is still alive.
func (m *Manager) IsRunning(sessionID string) bool {
	m.mu.Lock()
	_, ok := m.sessions[sessionID]
	m.mu.Unlock()
	return ok
}
