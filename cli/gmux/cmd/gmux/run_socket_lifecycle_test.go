package main

// run_socket_lifecycle_test.go — end-to-end acceptance for the defect this
// stack exists to fix: short `gmux -- cmd` launches must not leave canonical
// socket pathnames behind.
//
// This test builds and runs the *real* gmux binary. Nothing here reimplements
// the exit sequence, because the previous two attempts at covering this did
// exactly that: they replicated run.go's ordering in a helper, and deleting
// the production line they claimed to guard left the suite green. The only
// way to pin a line in a func that ends in os.Exit is to run the program.
//
// Mutation: delete the trailing srv.Shutdown() from runSession in run.go.
// Every launch then leaves its socket pathname behind and this test fails on
// the first iteration.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/socklease"
)

var (
	gmuxBinOnce sync.Once
	gmuxBinPath string
	gmuxBinErr  error
)

// buildGmuxBinary compiles the real gmux command once per test binary.
func buildGmuxBinary(t *testing.T) string {
	t.Helper()
	gmuxBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gmux-bin-")
		if err != nil {
			gmuxBinErr = err
			return
		}
		gmuxBinPath = filepath.Join(dir, "gmux")
		cmd := exec.Command("go", "build", "-o", gmuxBinPath, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			gmuxBinErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if gmuxBinErr != nil {
		t.Fatalf("building gmux: %v", gmuxBinErr)
	}
	return gmuxBinPath
}

// fakeDaemon is the smallest gmuxd the runner will accept: healthy, and
// willing to record registrations and deregistrations.
type fakeDaemon struct {
	requests     atomic.Int64
	registered   atomic.Int64
	deregistered atomic.Int64

	mu       sync.Mutex
	sockets  map[string]string // session id -> socket path, as registered
	deleted  map[string]bool
	srv      *http.Server
	listener net.Listener
}

func startFakeDaemon(t *testing.T, sockPath string) *fakeDaemon {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	d := &fakeDaemon{sockets: map[string]string{}, deleted: map[string]bool{}, listener: ln}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		d.requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": "dev"}})
	})
	mux.HandleFunc("POST /v1/register", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		var body struct {
			SessionID  string `json:"session_id"`
			SocketPath string `json:"socket_path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		d.sockets[body.SessionID] = body.SocketPath
		d.mu.Unlock()
		d.registered.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/deregister", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		var body struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		d.deleted[body.SessionID] = true
		d.mu.Unlock()
		d.deregistered.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	d.srv = &http.Server{Handler: mux}
	go func() { _ = d.srv.Serve(ln) }()
	t.Cleanup(func() { _ = d.srv.Close() })
	return d
}

func (d *fakeDaemon) registrations() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.sockets))
	for k, v := range d.sockets {
		out[k] = v
	}
	return out
}

func (d *fakeDaemon) deregistrations() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]bool, len(d.deleted))
	for k, v := range d.deleted {
		out[k] = v
	}
	return out
}

// launchEnv isolates a run completely: its own state directory (so the fake
// daemon's socket, and any session metadata, live in the test's tempdir) and
// its own runner socket directory.
func launchEnv(stateHome, socketDir string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "XDG_STATE_HOME="),
			strings.HasPrefix(kv, "GMUX_SOCKET_DIR="),
			strings.HasPrefix(kv, "GMUX_SESSION_ID="),
			strings.HasPrefix(kv, "GMUX_SOCKET="),
			strings.HasPrefix(kv, "_GMXINTERNAL_HANDSHAKE_FD="):
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"XDG_STATE_HOME="+stateHome,
		"GMUX_SOCKET_DIR="+socketDir,
		// Deterministic, adapter-free child.
		"SHELL=/bin/sh",
	)
}

// leftoverSockets lists per-session artifacts still in the runner socket directory.
func leftoverSockets(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read socket dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() == ".gmux-namespace.lock" {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// TestShortLaunchesLeaveNoSocketPathnames runs 100 real, short sessions and
// requires the socket directory to be empty afterwards -- no sockets, and no
// lock files either, since a lock file per session ever started is the same
// unbounded growth in a different guise.
func TestShortLaunchesLeaveNoSocketPathnames(t *testing.T) {
	bin := buildGmuxBinary(t)
	stateHome := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "sessions")
	daemon := startFakeDaemon(t, filepath.Join(stateHome, "gmux", "gmuxd.sock"))
	env := launchEnv(stateHome, socketDir)

	const launches = 100
	for i := range launches {
		cmd := exec.Command(bin, "--", "true")
		cmd.Env = env
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("launch %d: %v\nstdout: %s\nstderr: %s", i, err, stdout.String(), stderr.String())
		}
		// The payload rule: stdout carries only the child's output (`true`
		// prints nothing), the bare session id goes to stderr.
		if stdout.String() != "" {
			t.Fatalf("launch %d wrote non-payload bytes to stdout: %q", i, stdout.String())
		}
		id := strings.TrimSpace(stderr.String())
		if len(id) != 8 {
			t.Fatalf("launch %d did not report a bare 8-character session id on stderr: %q", i, stderr.String())
		}
		// The socket pathname must be gone the moment the process is: the
		// runner owns it, and nothing else is going to clean it up.
		if left := leftoverSockets(t, socketDir); len(left) != 0 {
			t.Fatalf("launch %d left %v in the socket directory; "+
				"the runner exited without releasing its socket pathname", i, left)
		}
	}

	if got := daemon.registered.Load(); got != launches {
		t.Errorf("registrations = %d, want %d", got, launches)
	}
	if got := daemon.deregistered.Load(); got != launches {
		t.Errorf("deregistrations = %d, want %d", got, launches)
	}
	// Every registration must have been for a socket in the isolated dir,
	// and every registered session must also have deregistered.
	deregistered := daemon.deregistrations()
	for id, sock := range daemon.registrations() {
		if filepath.Dir(sock) != socketDir {
			t.Errorf("session %s registered socket %s outside %s", id, sock, socketDir)
		}
		if !deregistered[id] {
			t.Errorf("session %s registered but never deregistered", id)
		}
	}
}

// A launch must also hand the pathname over cleanly enough that the very next
// launch can take the same session id -- the restart/resume path (ADR 0003).
func TestResumedSessionIDRebindsAfterExit(t *testing.T) {
	bin := buildGmuxBinary(t)
	stateHome := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "sessions")
	startFakeDaemon(t, filepath.Join(stateHome, "gmux", "gmuxd.sock"))
	env := append(launchEnv(stateHome, socketDir), "_GMXINTERNAL_RESUME_ID=10khtpym")

	for i := range 5 {
		cmd := exec.Command(bin, "--", "true")
		cmd.Env = env
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("launch %d: %v\nstdout: %s\nstderr: %s", i, err, stdout.String(), stderr.String())
		}
		// The resume id must be honoured every time. A leaked lease would
		// push the runner onto a freshly minted id instead. The runner reports
		// its session id on stderr.
		if strings.TrimSpace(stderr.String()) != "10khtpym" {
			t.Fatalf("launch %d did not bind the resumed id:\nstderr: %s", i, stderr.String())
		}
		if left := leftoverSockets(t, socketDir); len(left) != 0 {
			t.Fatalf("launch %d left %v behind", i, left)
		}
	}
}

// A runner killed outright (SIGKILL: no cleanup runs at all) is the population
// the daemon's reaper exists for. This pins the two halves of that contract
// from the runner's side: the pathname survives the kill, and the lease does
// not -- so the daemon can tell the difference between a dead runner and a
// live one.
func TestKilledRunnerLeavesReapableSocket(t *testing.T) {
	bin := buildGmuxBinary(t)
	stateHome := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "sessions")
	startFakeDaemon(t, filepath.Join(stateHome, "gmux", "gmuxd.sock"))

	cmd := exec.Command(bin, "--", "sleep", "60")
	cmd.Env = launchEnv(stateHome, socketDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// Wait for the socket to appear.
	var sock string
	deadline := time.Now().Add(20 * time.Second)
	for sock == "" {
		if time.Now().After(deadline) {
			t.Fatal("runner never bound a socket")
		}
		for _, name := range leftoverSockets(t, socketDir) {
			if strings.HasSuffix(name, ".sock") {
				sock = filepath.Join(socketDir, name)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = stdout

	// While the runner lives, the lease is held.
	if _, err := socklease.Acquire(sock); err == nil {
		t.Fatal("the lease was free while the runner was alive")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	// After the kill: the pathname is still there (nothing unlinked it) and
	// the lease is free (the kernel released it). That combination is exactly
	// what the daemon requires before it will reap.
	if _, ok := socklease.StatSocket(sock); !ok {
		t.Fatal("the socket pathname vanished on SIGKILL; the reaper's premise is wrong")
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		lease, err := socklease.AcquireExisting(sock)
		if err == nil {
			_ = lease.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease never became free after SIGKILL: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
