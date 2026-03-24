//go:build integration

// Harness provides a full gmuxd test environment for adapter integration tests.
// It starts a real gmuxd, launches sessions via the API, and observes state
// transitions via SSE polling.

package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// Session mirrors the gmuxd session schema fields we care about.
type Session struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Alive        bool     `json:"alive"`
	Pid          int      `json:"pid"`
	Title        string   `json:"title"`
	AdapterTitle string   `json:"adapter_title"`
	Cwd          string   `json:"cwd"`
	SocketPath   string   `json:"socket_path"`
	Status       *Status  `json:"status"`
	Resumable    bool     `json:"resumable"`
	ResumeKey    string   `json:"resume_key"`
	Command      []string `json:"command"`
}

type Status struct {
	Label   string `json:"label"`
	Working bool   `json:"working"`
}

// Gmuxd manages a gmuxd process for testing.
type Gmuxd struct {
	Addr string
	t    *testing.T
	cmd  *exec.Cmd
	cancel context.CancelFunc
}

// StartGmuxd starts a real gmuxd with isolated socket dir and port.
// The binaries must be pre-built at bin/gmux and bin/gmuxd.
func StartGmuxd(t *testing.T) *Gmuxd {
	t.Helper()
	repoRoot := findRepoRoot(t)
	gmuxdBin := filepath.Join(repoRoot, "bin", "gmuxd")
	gmuxBin := filepath.Join(repoRoot, "bin", "gmux")

	for _, bin := range []string{gmuxdBin, gmuxBin} {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("binary not found at %s — run scripts/build.sh first", bin)
		}
	}

	socketDir := t.TempDir()
	port := freePort(t)
	addr := fmt.Sprintf("http://localhost:%d", port)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, gmuxdBin, "start")
	configDir := t.TempDir() // empty config — no tailscale, no custom settings
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GMUXD_PORT=%d", port),
		fmt.Sprintf("GMUX_SOCKET_DIR=%s", socketDir),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", configDir),
		fmt.Sprintf("PATH=%s:%s", filepath.Dir(gmuxBin), os.Getenv("PATH")),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start gmuxd: %v", err)
	}

	g := &Gmuxd{Addr: addr, t: t, cmd: cmd, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		cmd.Process.Kill()
		cmd.Wait()
	})

	waitForHTTP(t, addr+"/v1/health", 5*time.Second)
	return g
}

// Launch starts a session and waits for it to appear alive.
func (g *Gmuxd) Launch(command []string, cwd string) Session {
	g.t.Helper()
	cmdJSON, _ := json.Marshal(command)
	body := fmt.Sprintf(`{"command":%s,"cwd":%q}`, cmdJSON, cwd)
	resp, err := http.Post(g.Addr+"/v1/launch", "application/json", strings.NewReader(body))
	if err != nil {
		g.t.Fatalf("launch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		g.t.Fatalf("launch: status %d", resp.StatusCode)
	}

	return g.WaitFor(func(s Session) bool {
		return s.Alive && s.Cwd == cwd && s.Kind != "" && s.SocketPath != ""
	}, 15*time.Second, "session to appear alive")
}

// Sessions returns all current sessions.
func (g *Gmuxd) Sessions() []Session {
	g.t.Helper()
	resp, err := http.Get(g.Addr + "/v1/sessions")
	if err != nil {
		g.t.Fatalf("list sessions: %v", err)
	}
	defer resp.Body.Close()
	var env struct{ Data []Session `json:"data"` }
	json.NewDecoder(resp.Body).Decode(&env)
	return env.Data
}

// GetSession returns a specific session by ID.
func (g *Gmuxd) GetSession(id string) (Session, bool) {
	for _, s := range g.Sessions() {
		if s.ID == id {
			return s, true
		}
	}
	return Session{}, false
}

// WaitFor polls all sessions until pred matches one, or times out.
func (g *Gmuxd) WaitFor(pred func(Session) bool, timeout time.Duration, desc string) Session {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, s := range g.Sessions() {
			if pred(s) {
				return s
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	g.t.Fatalf("timeout waiting for %s", desc)
	return Session{}
}

// WaitForSession polls until pred matches for a specific session ID.
func (g *Gmuxd) WaitForSession(id string, pred func(Session) bool, timeout time.Duration, desc string) Session {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, ok := g.GetSession(id); ok && pred(s) {
			return s
		}
		time.Sleep(300 * time.Millisecond)
	}
	g.t.Fatalf("timeout waiting for session %s: %s", id, desc)
	return Session{}
}

func (g *Gmuxd) Kill(id string) {
	g.t.Helper()
	resp, _ := http.Post(g.Addr+"/v1/sessions/"+id+"/kill", "", nil)
	if resp != nil { resp.Body.Close() }
}

func (g *Gmuxd) Dismiss(id string) {
	g.t.Helper()
	resp, _ := http.Post(g.Addr+"/v1/sessions/"+id+"/dismiss", "", nil)
	if resp != nil { resp.Body.Close() }
}

func (g *Gmuxd) Resume(id string) {
	g.t.Helper()
	resp, _ := http.Post(g.Addr+"/v1/sessions/"+id+"/resume", "", nil)
	if resp != nil { resp.Body.Close() }
}

// ConnectSession opens a persistent WebSocket directly to the session's runner
// (via its Unix socket), bypassing gmuxd's WS proxy. This is more reliable for
// tests — fewer moving parts. Returns a send function; cleanup is automatic.
func (g *Gmuxd) ConnectSession(sessionID string) (send func(data string), close func()) {
	g.t.Helper()

	sess, ok := g.GetSession(sessionID)
	if !ok {
		g.t.Fatalf("ConnectSession: session %s not found", sessionID)
	}
	if sess.SocketPath == "" {
		g.t.Fatalf("ConnectSession: session %s has no socket", sessionID)
	}

	ctx, cancel := context.WithCancel(context.Background())

	conn, _, err := websocket.Dial(ctx, "ws://localhost/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sess.SocketPath)
				},
			},
		},
	})
	if err != nil {
		cancel()
		g.t.Fatalf("ws dial runner %s: %v", sessionID, err)
	}

	// Set terminal size so the PTY renders TUI apps properly.
	resizeMsg := `{"type":"resize","cols":80,"rows":24}`
	wCtx, wCancel := context.WithTimeout(ctx, 3*time.Second)
	conn.Write(wCtx, websocket.MessageText, []byte(resizeMsg))
	wCancel()

	// Drain incoming messages in background (scrollback replay + terminal output).
	go func() {
		for {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
		}
	}()

	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			conn.Close(websocket.StatusNormalClosure, "done")
			cancel()
		})
	}
	g.t.Cleanup(closeFn)

	sendFn := func(data string) {
		wCtx, wCancel := context.WithTimeout(ctx, 5*time.Second)
		defer wCancel()
		if err := conn.Write(wCtx, websocket.MessageBinary, []byte(data)); err != nil {
			g.t.Logf("ws write warning: %v", err)
		}
	}

	return sendFn, closeFn
}

// ── Helpers ──

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.work)")
		}
		dir = parent
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil { t.Fatal(err) }
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 { return }
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", url)
}

func unixClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 3 * time.Second,
	}
}

// RequireBinary skips the test if the binary is not on PATH.
func RequireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found on PATH, skipping", name)
	}
}

// ReadScrollback reads the terminal scrollback text from a session's runner.
func ReadScrollback(t *testing.T, socketPath string) string {
	t.Helper()
	client := unixClient(socketPath)
	resp, err := client.Get("http://localhost/scrollback/text")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// WaitForScrollback polls scrollback until it contains the expected string.
func (g *Gmuxd) WaitForScrollback(socketPath, substr string, timeout time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text := ReadScrollback(g.t, socketPath)
		if strings.Contains(text, substr) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	text := ReadScrollback(g.t, socketPath)
	g.t.Fatalf("timeout waiting for %q in scrollback. Got:\n%.500s", substr, text)
}

// WaitForOutput polls the session's scrollback until it contains non-trivial
// content (indicating the TUI has rendered). Returns the scrollback text.
func (g *Gmuxd) WaitForOutput(sessionID string, timeout time.Duration) string {
	g.t.Helper()
	sess, ok := g.GetSession(sessionID)
	if !ok {
		g.t.Fatalf("session %s not found", sessionID)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client := unixClient(sess.SocketPath)
		resp, err := client.Get("http://localhost/scrollback/text")
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			text := strings.TrimSpace(string(data))
			if len(text) > 10 { // non-trivial output
				return text
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	g.t.Fatalf("timeout waiting for output from session %s", sessionID)
	return ""
}
