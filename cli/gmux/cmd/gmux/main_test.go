package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

func TestGmuxdNeedsStart_ReleaseMissingVersionPreservesOwner(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	sockDir := filepath.Join(stateDir, "gmux")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "gmuxd.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{}}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	if gmuxdNeedsStart() {
		t.Fatal("missing health version triggered implicit release takeover")
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

func TestGmuxdNeedsStart_AcceptedSocketIsAliveWithoutHealthResponse(t *testing.T) {
	old := version
	version = "dev"
	defer func() { version = old }()

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	sockDir := filepath.Join(stateDir, "gmux")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(sockDir, "gmuxd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			close(accepted)
			defer conn.Close()
			<-stop // connected owner deliberately never speaks HTTP
		}
	}()

	start := time.Now()
	if gmuxdNeedsStart() {
		t.Fatal("accepted socket was misclassified as an absent daemon")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("socket liveness waited for HTTP: %v", elapsed)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("liveness probe never connected")
	}
}

func TestConcurrentAutostartSpawnsOneCandidate(t *testing.T) {
	oldVersion := version
	version = "dev"
	defer func() { version = oldVersion }()

	base := t.TempDir()
	state := filepath.Join(base, "state")
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	countPath := filepath.Join(base, "spawns")
	pidPath := filepath.Join(base, "pid")
	t.Setenv("FAKE_COUNT", countPath)
	t.Setenv("FAKE_PID", pidPath)
	t.Setenv("FAKE_SOCKET", filepath.Join(state, "gmux", "gmuxd.sock"))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	script := `#!/bin/sh
set -eu
printf 'spawn\n' >> "$FAKE_COUNT"
exec python3 - <<'PY'
import os, socket
p=os.environ['FAKE_SOCKET']
os.makedirs(os.path.dirname(p), exist_ok=True)
try: os.unlink(p)
except FileNotFoundError: pass
s=socket.socket(socket.AF_UNIX)
s.bind(p); s.listen(64)
open(os.environ['FAKE_PID'],'w').write(str(os.getpid()))
while True:
 c,_=s.accept(); c.close()
PY
`
	if err := os.WriteFile(filepath.Join(bin, "gmuxd"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidPath); err == nil {
			var pid int
			_, _ = fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid)
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ensureGmuxdContext(ctx)
		}()
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "spawn\n"); got != 1 {
		t.Fatalf("concurrent autostart spawned %d candidates, want 1", got)
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
