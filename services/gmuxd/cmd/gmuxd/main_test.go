package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

type discoverTestAdapter struct {
	name      string
	available bool
}

func (a discoverTestAdapter) Name() string                      { return a.name }
func (a discoverTestAdapter) Discover() bool                    { return a.available }
func (a discoverTestAdapter) Match(_ []string) bool             { return false }
func (a discoverTestAdapter) Env(_ adapter.EnvContext) []string { return nil }
func (a discoverTestAdapter) Monitor(_ []byte) *adapter.Status  { return nil }
func (a discoverTestAdapter) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{ID: a.name, Label: a.name}}
}

func TestDiscoverAvailableAdaptersRunsAll(t *testing.T) {
	available := discoverAvailableAdapters([]adapter.Adapter{
		discoverTestAdapter{name: "pi", available: true},
		discoverTestAdapter{name: "opencode", available: false},
		discoverTestAdapter{name: "shell", available: true},
	})

	if !available["pi"] {
		t.Fatal("expected pi to be available")
	}
	if available["opencode"] {
		t.Fatal("expected opencode to be unavailable")
	}
	if !available["shell"] {
		t.Fatal("expected shell to be available")
	}
}

func TestLaunchersForAdaptersFiltersUnavailable(t *testing.T) {
	adapterList := []adapter.Adapter{
		discoverTestAdapter{name: "pi", available: true},
		discoverTestAdapter{name: "opencode", available: false},
		discoverTestAdapter{name: "shell", available: true},
	}

	launchers := launchersForAdapters(adapterList, map[string]bool{
		"pi":       true,
		"opencode": false,
		"shell":    true,
	})

	if len(launchers) != 2 {
		t.Fatalf("expected 2 available launchers, got %#v", launchers)
	}
	for _, l := range launchers {
		if !l.Available {
			t.Fatalf("expected launcher to be available: %#v", l)
		}
		if l.ID == "opencode" {
			t.Fatalf("did not expect unavailable launcher in config: %#v", l)
		}
	}
	if launchers[0].ID != "pi" || launchers[1].ID != "shell" {
		t.Fatalf("unexpected launcher order: %#v", launchers)
	}
}

func TestDiscoverLaunchersUsesCompiledAdapters(t *testing.T) {
	cfg := discoverLaunchers()
	if cfg.DefaultLauncher != "shell" {
		t.Fatalf("expected default launcher shell, got %q", cfg.DefaultLauncher)
	}
	if len(cfg.Launchers) < 1 {
		t.Fatalf("expected at least 1 launcher, got %d", len(cfg.Launchers))
	}

	seenShell := false
	for _, l := range cfg.Launchers {
		if !l.Available {
			t.Fatalf("did not expect unavailable launcher in config: %#v", l)
		}
		if l.ID == "shell" {
			seenShell = true
		}
	}
	if !seenShell {
		t.Fatalf("expected shell launcher in %#v", cfg.Launchers)
	}
	if got := cfg.Launchers[len(cfg.Launchers)-1].ID; got != "shell" {
		t.Fatalf("expected shell last, got %q", got)
	}
}

func TestRunNoArgsPrintsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage: gmuxd") {
		t.Fatalf("expected usage output, got %q", stdout.String())
	}
}

func TestRunHelpCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "Usage: gmuxd") {
		t.Fatalf("expected usage output, got %q", got)
	}
}

func TestRunVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if got := stdout.String(); !strings.Contains(got, version) {
		t.Fatalf("expected version output, got %q", got)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "unknown command") {
		t.Fatalf("expected error output, got %q", got)
	}
}

func TestRunStartRejectsUnknownOption(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"start", "--wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "unknown option") {
		t.Fatalf("expected unknown option error, got %q", got)
	}
}

func TestRunRunRejectsUnknownOption(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"run", "--wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "unknown option") {
		t.Fatalf("expected unknown option error, got %q", got)
	}
}

func TestRunStopNoRunningDaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"stop"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "no running daemon") {
		t.Fatalf("expected 'no running daemon', got %q", stdout.String())
	}
}

func TestRunStatusNoRunningDaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("expected 'not running', got %q", stderr.String())
	}
}

func TestRunStatusWithRunningDaemon(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemon(t, "0.9.0")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "0.9.0") {
		t.Errorf("expected version in output, got %q", out)
	}
	if !strings.Contains(out, "socket:") {
		t.Errorf("expected socket path in output, got %q", out)
	}
}

func TestRunStatusShowsSessionsAndPeers(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemonFull(t)
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()

	// Session summary with local/remote breakdown.
	if !strings.Contains(out, "3 alive (2 local, 1 remote)") {
		t.Errorf("expected session summary, got %q", out)
	}
	if !strings.Contains(out, "5 dead") {
		t.Errorf("expected dead count, got %q", out)
	}

	// Connected peer with session count.
	if !strings.Contains(out, "desktop (1 session)") {
		t.Errorf("expected connected peer, got %q", out)
	}

	// Disconnected peer with error.
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected disconnected peer error, got %q", out)
	}
}

func TestRunAuthNoRunningDaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"auth"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("expected 'not running', got %q", stderr.String())
	}
}

func TestRunAuthWithRunningDaemon(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemon(t, "0.9.0")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"auth"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "127.0.0.1:8790") {
		t.Errorf("expected listen address in output, got %q", out)
	}
	if !strings.Contains(out, "/auth/login?token=") {
		t.Errorf("expected auth URL in output, got %q", out)
	}
	if !strings.Contains(out, "test-token-abc") {
		t.Errorf("expected token in output, got %q", out)
	}
}

func TestUsageIncludesNewCommands(t *testing.T) {
	var stdout bytes.Buffer
	printUsage(&stdout)
	out := stdout.String()
	for _, cmd := range []string{"start", "stop", "status", "auth", "remote"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage missing command %q", cmd)
		}
	}
	// Old commands should not appear.
	for _, old := range []string{"shutdown", "auth-link", "--replace"} {
		if strings.Contains(out, old) {
			t.Errorf("usage should not contain old command %q", old)
		}
	}
}

// startTestSocketDaemon starts a minimal HTTP server on a Unix socket
// at the standard SocketPath() location under a temp XDG_STATE_HOME.
// Returns the state dir (for t.Setenv) and a cleanup func.
func startTestSocketDaemon(t *testing.T, ver string) (stateDir string, cleanup func()) {
	t.Helper()
	stateDir = t.TempDir()
	sockDir := filepath.Join(stateDir, "gmux")
	os.MkdirAll(sockDir, 0o700)
	sockPath := filepath.Join(sockDir, "gmuxd.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := map[string]any{
			"service":    "gmuxd",
			"version":    ver,
			"status":     "ready",
			"listen":     "127.0.0.1:8790",
			"auth_token": "test-token-abc",
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	return stateDir, func() {
		srv.Close()
		os.Remove(sockPath)
	}
}

// startTestSocketDaemonFull starts a mock daemon that returns sessions and peers.
func startTestSocketDaemonFull(t *testing.T) (stateDir string, cleanup func()) {
	t.Helper()
	stateDir = t.TempDir()
	sockDir := filepath.Join(stateDir, "gmux")
	os.MkdirAll(sockDir, 0o700)
	sockPath := filepath.Join(sockDir, "gmuxd.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := map[string]any{
			"service":    "gmuxd",
			"version":    "1.0.0",
			"status":     "ready",
			"listen":     "127.0.0.1:8790",
			"auth_token": "test-token-abc",
			"sessions": map[string]int{
				"local_alive":  2,
				"remote_alive": 1,
				"dead":         5,
			},
			"peers": []map[string]any{
				{"name": "desktop", "url": "https://desktop.ts.net", "status": "connected", "session_count": 1},
				{"name": "server", "url": "https://server.ts.net", "status": "disconnected", "session_count": 0, "last_error": "connection refused"},
			},
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	return stateDir, func() {
		srv.Close()
		os.Remove(sockPath)
	}
}

// Verify unixipc package is properly usable.
func TestUnixIPCReplace(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	// Replace on nonexistent socket should succeed.
	if err := unixipc.Replace(sockPath); err != nil {
		t.Fatal(err)
	}
}

func TestRunSubcommandHelp(t *testing.T) {
	tests := []struct {
		cmd      string
		contains string
	}{
		{"start", "background"},
		{"run", "foreground"},
		{"restart", "background"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		code := run([]string{tt.cmd, "--help"}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%s --help: exit code %d, want 0", tt.cmd, code)
		}
		if !strings.Contains(stdout.String(), tt.contains) {
			t.Errorf("%s --help: expected %q in output, got %q", tt.cmd, tt.contains, stdout.String())
		}
	}
}
