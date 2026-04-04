// Package e2e tests the full gmux pipeline:
//
//	gmux-run → register → gmuxd /v1/sessions → PUT /status → shutdown
//
// This builds both binaries, starts them as subprocesses, and verifies
// the data flow via HTTP.
package e2e

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
	"testing"
	"time"
)

// session mirrors the schema v2 fields we care about.
type session struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Alive      bool    `json:"alive"`
	Pid        int     `json:"pid"`
	Title      string  `json:"title"`
	Cwd        string  `json:"cwd"`
	SocketPath string  `json:"socket_path"`
	Status     *status `json:"status"`
}

type status struct {
	Label string `json:"label"`
	State string `json:"state"`
}

type envelope struct {
	OK   bool      `json:"ok"`
	Data []session `json:"data"`
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	repoRoot := findRepoRoot(t)
	gmuxdBin := buildBinary(t, repoRoot, "services/gmuxd/cmd/gmuxd")
	gmuxRunBin := buildBinary(t, repoRoot, "cli/gmux/cmd/gmux")

	// Use a temp dir for sockets to avoid polluting /tmp
	socketDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", socketDir)

	// State dir for Unix socket and auth token
	stateDir := t.TempDir()
	gmuxdSock := filepath.Join(stateDir, "gmux", "gmuxd.sock")

	// Use a free port for the TCP listener
	port := freePort(t)
	configDir := t.TempDir()
	cfgDir := filepath.Join(configDir, "gmux")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "host.toml"),
		[]byte(fmt.Sprintf("port = %d\n", port)), 0o644)

	// ── Start gmuxd ──
	gmuxdCtx, gmuxdCancel := context.WithCancel(context.Background())
	defer gmuxdCancel()

	gmuxdCmd := exec.CommandContext(gmuxdCtx, gmuxdBin)
	gmuxdCmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", configDir),
		fmt.Sprintf("XDG_STATE_HOME=%s", stateDir),
	)
	gmuxdCmd.Stdout = os.Stdout
	gmuxdCmd.Stderr = os.Stderr
	if err := gmuxdCmd.Start(); err != nil {
		t.Fatalf("start gmuxd: %v", err)
	}
	defer gmuxdCmd.Process.Kill()

	// Wait for gmuxd to be ready via Unix socket
	waitForSocket(t, gmuxdSock, 5*time.Second)

	// Verify empty sessions
	sessions := listSessions(t, gmuxdSock)
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}

	// ── Start gmux-run ──
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	runCmd := exec.CommandContext(runCtx, gmuxRunBin, "bash", "-c", "echo hello-from-e2e; sleep 60")
	runCmd.Dir = repoRoot
	runCmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_STATE_HOME=%s", stateDir),
	)

	// Capture stdout to extract session ID and socket path
	runStdout, err := runCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	runCmd.Stderr = os.Stderr

	if err := runCmd.Start(); err != nil {
		t.Fatalf("start gmux-run: %v", err)
	}
	defer runCmd.Process.Kill()

	// Parse gmux-run output for session ID and socket path
	sessionID, socketPath := parseRunOutput(t, runStdout)
	t.Logf("session: %s, socket: %s", sessionID, socketPath)

	// ── Verify session appears in gmuxd ──
	var found *session
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sessions = listSessions(t, gmuxdSock)
		for i := range sessions {
			if sessions[i].ID == sessionID {
				found = &sessions[i]
				break
			}
		}
		if found != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if found == nil {
		t.Fatal("session not found in gmuxd after 10s")
	}

	t.Logf("session found: %+v", found)

	// Verify session fields
	if !found.Alive {
		t.Error("expected alive=true")
	}
	if found.Kind != "generic" {
		t.Errorf("expected kind=generic, got %q", found.Kind)
	}
	if found.Pid == 0 {
		t.Error("expected non-zero pid")
	}
	if found.SocketPath == "" {
		t.Error("expected socket_path")
	}
	if !strings.Contains(found.Cwd, "gmux") {
		t.Errorf("expected cwd containing 'gmux', got %q", found.Cwd)
	}

	// ── Test PUT /status on runner socket ──
	putStatus(t, socketPath, `{"label":"e2e-test","state":"attention"}`)

	// Verify status updated via runner /meta
	meta := getRunnerMeta(t, socketPath)
	if meta.Status == nil || meta.Status.Label != "e2e-test" || meta.Status.State != "attention" {
		t.Errorf("expected status {e2e-test, attention}, got %+v", meta.Status)
	}

	// Wait for gmuxd to pick up the status change (next scan)
	time.Sleep(4 * time.Second)
	sessions = listSessions(t, gmuxdSock)
	for i := range sessions {
		if sessions[i].ID == sessionID {
			found = &sessions[i]
			break
		}
	}
	if found.Status == nil || found.Status.Label != "e2e-test" {
		t.Errorf("gmuxd did not pick up status change: %+v", found.Status)
	}

	// ── Test PATCH /meta ──
	patchMeta(t, socketPath, `{"title":"updated-title","subtitle":"sub"}`)
	meta = getRunnerMeta(t, socketPath)
	if meta.Title != "updated-title" {
		t.Errorf("expected title 'updated-title', got %q", meta.Title)
	}

	// ── Kill runner, verify cleanup ──
	runCmd.Process.Kill()
	runCmd.Wait()

	// Give gmuxd time to scan and detect stale socket
	time.Sleep(5 * time.Second)
	sessions = listSessions(t, gmuxdSock)
	for _, s := range sessions {
		if s.ID == sessionID {
			t.Error("session should have been removed after runner exit")
		}
	}

	t.Log("end-to-end test passed")
}

// ── Helpers ──

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Walk up to find go.work
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

func buildBinary(t *testing.T, repoRoot, pkg string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", binPath, "./"+pkg)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", pkg, err)
	}
	return binPath
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func waitForSocket(t *testing.T, sockPath string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, time.Second)
			},
		},
		Timeout: 2 * time.Second,
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://localhost/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for gmuxd on socket %s", sockPath)
}

func parseRunOutput(t *testing.T, r io.Reader) (sessionID, socketPath string) {
	t.Helper()
	buf := make([]byte, 4096)
	var collected string
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		n, _ := r.Read(buf)
		if n > 0 {
			collected += string(buf[:n])
		}
		// Look for both session and socket lines
		for _, line := range strings.Split(collected, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "session:") {
				sessionID = strings.TrimSpace(strings.TrimPrefix(line, "session:"))
			}
			if strings.HasPrefix(line, "socket:") {
				socketPath = strings.TrimSpace(strings.TrimPrefix(line, "socket:"))
			}
		}
		if sessionID != "" && socketPath != "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("could not parse session ID and socket from gmux-run output. Got: %q", collected)
	return
}

func listSessions(t *testing.T, sockPath string) []session {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, time.Second)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://localhost/v1/sessions")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	defer resp.Body.Close()

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	return env.Data
}

func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 3 * time.Second,
	}
}

func putStatus(t *testing.T, socketPath, body string) {
	t.Helper()
	client := unixHTTPClient(socketPath)
	req, _ := http.NewRequest("PUT", "http://localhost/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT /status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("PUT /status: expected 204, got %d", resp.StatusCode)
	}
}

func patchMeta(t *testing.T, socketPath, body string) {
	t.Helper()
	client := unixHTTPClient(socketPath)
	req, _ := http.NewRequest("PATCH", "http://localhost/meta", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH /meta: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("PATCH /meta: expected 204, got %d", resp.StatusCode)
	}
}

func getRunnerMeta(t *testing.T, socketPath string) session {
	t.Helper()
	client := unixHTTPClient(socketPath)
	resp, err := client.Get("http://localhost/meta")
	if err != nil {
		t.Fatalf("GET /meta: %v", err)
	}
	defer resp.Body.Close()

	var s session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return s
}
