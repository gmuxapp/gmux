package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startTestSocketDaemon starts a minimal gmuxd on a Unix socket
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
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"service": "gmuxd",
				"version": ver,
				"status":  "ready",
			},
		})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	return stateDir, func() {
		srv.Close()
		os.Remove(sockPath)
	}
}

func TestGmuxdNeedsStart_NotRunning(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if !gmuxdNeedsStart() {
		t.Error("expected true when daemon is unreachable")
	}
}

func TestGmuxdNeedsStart_SameVersion(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	stateDir, cleanup := startTestSocketDaemon(t, "0.4.4")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	if gmuxdNeedsStart() {
		t.Error("expected false when versions match")
	}
}

func TestGmuxdNeedsStart_OlderVersion(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	stateDir, cleanup := startTestSocketDaemon(t, "0.4.3")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	if !gmuxdNeedsStart() {
		t.Error("expected true when daemon is older")
	}
}

func TestGmuxdNeedsStart_NewerVersion(t *testing.T) {
	old := version
	version = "0.4.3"
	defer func() { version = old }()

	stateDir, cleanup := startTestSocketDaemon(t, "0.4.4")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	if !gmuxdNeedsStart() {
		t.Error("expected true when versions differ")
	}
}

func TestGmuxdNeedsStart_DevNeverReplaces(t *testing.T) {
	old := version
	version = "dev"
	defer func() { version = old }()

	stateDir, cleanup := startTestSocketDaemon(t, "0.4.3")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	if gmuxdNeedsStart() {
		t.Error("dev builds must not replace a healthy daemon")
	}
}

func TestGmuxdNeedsStart_DevStartsWhenNotRunning(t *testing.T) {
	old := version
	version = "dev"
	defer func() { version = old }()

	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if !gmuxdNeedsStart() {
		t.Error("expected true for dev build when daemon is not running")
	}
}

func TestParseHealthField(t *testing.T) {
	body := []byte(`{"ok":true,"data":{"listen":"127.0.0.1:8790","auth_token":"abc123","version":"1.0.0"}}`)

	if got := parseHealthField(body, "listen"); got != "127.0.0.1:8790" {
		t.Errorf("listen = %q", got)
	}
	if got := parseHealthField(body, "auth_token"); got != "abc123" {
		t.Errorf("auth_token = %q", got)
	}
	if got := parseHealthField(body, "nonexistent"); got != "" {
		t.Errorf("nonexistent = %q, want empty", got)
	}
}
