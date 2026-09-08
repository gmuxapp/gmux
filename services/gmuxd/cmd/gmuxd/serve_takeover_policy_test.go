package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

func TestIncumbentCheckIsFirstStartupOperation(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{"state", "config/gmux", "home", "run"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("GMUX_SOCKET_DIR", filepath.Join(base, "run"))
	// If startup gets past the incumbent check, config loading must fail.
	if err := os.WriteFile(filepath.Join(base, "config/gmux/host.toml"), []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := unixipc.Listen(paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": version, "pid": 42}})
	})}
	go srv.Serve(ln)
	defer srv.Close()

	if code := serveCentral(io.Discard, false); code != 0 {
		t.Fatalf("serve reached bootstrap after incumbent check: exit %d", code)
	}
	for _, name := range []string{"auth-token", "node-id", "state.db"} {
		if _, err := os.Stat(filepath.Join(paths.StateDir(), name)); !os.IsNotExist(err) {
			t.Fatalf("startup created %s before yielding: %v", name, err)
		}
	}
}

// TestServeRefusesToReplaceHealthySameVersionIncumbent pins the takeover
// policy that ended the autostart incident: `gmux`'s daemon autostart spawns
// `gmuxd run` whenever a health probe times out, and the old unconditional
// takeover made every such spawn SHUT DOWN the healthy production daemon —
// a rolling outage under load. A non-replace serve invocation against a
// healthy same-version incumbent must exit 0 without disturbing it; an
// explicit --replace (gmuxd start/restart) must still win.
func TestServeRefusesToReplaceHealthySameVersionIncumbent(t *testing.T) {
	base, err := os.MkdirTemp("", "takeover-policy-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	for _, dir := range []string{"state", "config", "home", "run"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("GMUX_SOCKET_DIR", filepath.Join(base, "run"))
	port := freePort(t)
	cfgDir := filepath.Join(base, "config", "gmux")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("port = %d\n[discovery]\ndevcontainers = false\n[tailscale]\nenabled = false\n", port)
	if err := os.WriteFile(filepath.Join(cfgDir, "host.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	incumbentDone := make(chan int, 1)
	go func() { incumbentDone <- serveCentral(io.Discard, false) }()
	sock := paths.SocketPath()
	waitUntil(t, 10*time.Second, func() bool { return unixipc.Healthy(sock) }, "incumbent never became healthy")

	// Point any new invocation's Pi source at a FIFO. Reading it would block,
	// making this an ordering guard: a same-version challenger must complete
	// the incumbent precheck before attempting the expensive conversation
	// snapshot. The incumbent's watcher already captured the original root.
	blockedAgentDir := filepath.Join(base, "blocked-agent")
	blockedSessions := filepath.Join(blockedAgentDir, "sessions")
	if err := os.MkdirAll(blockedSessions, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(blockedSessions, "would-block.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", blockedAgentDir)

	// Same version (both "dev" in tests), no --replace: must yield exit 0
	// quickly, without opening the FIFO, and leave the incumbent running.
	challenger := make(chan int, 1)
	go func() { challenger <- serveCentral(io.Discard, false) }()
	select {
	case code := <-challenger:
		if code != 0 {
			t.Fatalf("non-replace serve against healthy incumbent: exit %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("non-replace serve did not yield to the incumbent")
	}
	if !unixipc.Healthy(sock) {
		t.Fatal("incumbent was disturbed by the non-replace challenger")
	}
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}

	// Explicit --replace must shut the incumbent down and take over.
	replacer := make(chan int, 1)
	go func() { replacer <- serveCentral(io.Discard, true) }()
	select {
	case <-incumbentDone:
		// incumbent exited: replacement proceeded
	case <-time.After(15 * time.Second):
		t.Fatal("--replace did not shut down the incumbent")
	}
	waitUntil(t, 15*time.Second, func() bool { return unixipc.Healthy(sock) }, "replacement daemon never became healthy")

	// Shut the replacement down so the test leaves nothing behind.
	if !unixipc.Shutdown(sock) {
		t.Fatal("could not shut down replacement daemon")
	}
	select {
	case <-replacer:
	case <-time.After(10 * time.Second):
		t.Fatal("replacement daemon did not exit after shutdown")
	}
}

// Source builds carry a per-tree stamp (dev+<hash>), so two rebuilds of the
// same tree are *different strings* while still being the same kind of build.
// Before the stamp existed, every source daemon said "dev" and implicit
// replacement was unreachable here; with it, plain string equality would let a
// bare `gmuxd run` shut a healthy incumbent down without --replace — in either
// direction, since an older tree can carry the newer hash. Both directions and
// the mixed stamped/unstamped case must yield instead.
func TestImplicitIncumbentCheckTreatsDevStampsAsOneVersion(t *testing.T) {
	cases := []struct {
		name            string
		mine, incumbent string
		wantYield       bool
	}{
		{"different dev stamps", "dev+aaaaaaaaaaaa", "dev+bbbbbbbbbbbb", true},
		{"reverse direction", "dev+bbbbbbbbbbbb", "dev+aaaaaaaaaaaa", true},
		{"stamped vs unstamped", "dev+aaaaaaaaaaaa", "dev", true},
		{"dirty stamp", "dev+aaaaaaaaaaaa", "dev+aaaaaaaaaaaa-dirty", true},
		{"same release", "v2.1.1", "v2.1.1", true},
		{"real version difference", "v2.1.1", "v2.1.0", false},
		{"release vs dev", "v2.1.1", "dev+aaaaaaaaaaaa", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := version
			version = tc.mine
			defer func() { version = old }()

			base := t.TempDir()
			for _, dir := range []string{"state", "home", "run"} {
				if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", filepath.Join(base, "home"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
			t.Setenv("GMUX_SOCKET_DIR", filepath.Join(base, "run"))
			sock := paths.SocketPath()
			if !strings.HasPrefix(sock, base) {
				t.Fatalf("socket path escaped the test sandbox: %s", sock)
			}
			ln, err := unixipc.Listen(sock)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{"version": tc.incumbent, "pid": 4242},
				})
			})}
			go srv.Serve(ln)
			defer srv.Close()

			err = implicitIncumbentCheck(sock)
			if tc.wantYield && err == nil {
				t.Fatalf("challenger %s replaced healthy incumbent %s without --replace", tc.mine, tc.incumbent)
			}
			if !tc.wantYield && err != nil {
				t.Fatalf("challenger %s refused to replace incumbent %s: %v", tc.mine, tc.incumbent, err)
			}
			if tc.wantYield && !errors.Is(err, errIncumbentHealthy) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
