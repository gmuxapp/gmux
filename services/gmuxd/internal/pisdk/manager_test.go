package pisdk

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"nhooyr.io/websocket"
)

// ─── subprocess test harness ─────────────────────────────────────────────────
//
// Tests spawn the test binary itself as a subprocess (standard Go pattern).
// The subprocess detects GO_WANT_HELPER_PROCESS=1 and the mode after "--".

// TestHelperProcess is invoked only by the test subprocess, not by the runner.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	mode := helperMode()
	switch mode {
	case "emit-session-ready":
		// Emit session_ready, then wait for stdin to close (graceful shutdown).
		fmt.Println(`{"type":"session_ready","model":"test-model"}`)
		io.Copy(io.Discard, os.Stdin) //nolint:errcheck
	case "emit-and-exit":
		// Emit session_ready then exit immediately (tests exit lifecycle).
		fmt.Println(`{"type":"session_ready","model":"m1"}`)
	case "echo-stdin":
		// Echo every byte received on stdin back to stdout (used for broadcast tests).
		io.Copy(os.Stdout, os.Stdin) //nolint:errcheck
	default:
		fmt.Fprintf(os.Stderr, "pisdk test helper: unknown mode %q\n", mode)
		os.Exit(1)
	}
	os.Exit(0)
}

func helperMode() string {
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// helperCmd returns the argv to run the test binary as a subprocess in mode.
func helperCmd(mode string) []string {
	return []string{os.Args[0], "-test.run=TestHelperProcess", "--", mode}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func newTestStore(id string) *store.Store {
	s := store.New()
	s.Upsert(store.Session{ID: id, Kind: "pi-sdk", Alive: true})
	return s
}

// waitFor polls cond every 10 ms until it returns true or the timeout elapses.
// Reports a fatal error if the timeout fires.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", desc)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// launchHelper starts a helper subprocess via m.Launch, setting the env var
// that activates the helper mode in the subprocess.
func launchHelper(t *testing.T, m *Manager, sessID, mode string) {
	t.Helper()
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	if err := m.Launch(sessID, helperCmd(mode)); err != nil {
		t.Fatalf("Launch(%s): %v", mode, err)
	}
}

// ─── tests ───────────────────────────────────────────────────────────────────

// TestSessionReadyUpdatesSubtitle verifies that a session_ready JSON line
// emitted by the subprocess causes the store to reflect the model as Subtitle.
func TestSessionReadyUpdatesSubtitle(t *testing.T) {
	s := newTestStore("sess-1")
	m := New(s)

	launchHelper(t, m, "sess-1", "emit-session-ready")

	waitFor(t, 5*time.Second, "subtitle == test-model", func() bool {
		sess, _ := s.Get("sess-1")
		return sess.Subtitle == "test-model"
	})

	// Cleanup: close stdin so the helper exits cleanly.
	m.mu.Lock()
	proc := m.sessions["sess-1"]
	m.mu.Unlock()
	if proc != nil {
		proc.stdin.Close()
	}
}

// TestBroadcastToConnectedClients verifies that JSON lines from the subprocess
// stdout are fanned out to all connected WebSocket clients.
func TestBroadcastToConnectedClients(t *testing.T) {
	s := newTestStore("sess-2")
	m := New(s)

	launchHelper(t, m, "sess-2", "echo-stdin")

	// Serve HandleWebSocket via httptest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.HandleWebSocket(w, r, "sess-2")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"

	// Connect two clients.
	ctx := context.Background()
	c1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("client 1 dial: %v", err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "")

	c2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("client 2 dial: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	// Wait until both conns are registered in proc.conns.
	waitFor(t, 3*time.Second, "2 WS clients registered", func() bool {
		m.mu.Lock()
		proc := m.sessions["sess-2"]
		m.mu.Unlock()
		if proc == nil {
			return false
		}
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return len(proc.conns) == 2
	})

	// Write a JSON line directly to subprocess stdin; it echoes to stdout,
	// which readLoop picks up and broadcasts.
	want := `{"type":"test","payload":"hello"}`
	m.mu.Lock()
	proc := m.sessions["sess-2"]
	m.mu.Unlock()
	if proc == nil {
		t.Fatal("subprocess not found")
	}
	if _, err := proc.stdin.Write([]byte(want + "\n")); err != nil {
		t.Fatalf("stdin write: %v", err)
	}

	readMsg := func(conn *websocket.Conn, label string) string {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, data, err := conn.Read(rctx)
		if err != nil {
			t.Errorf("%s read: %v", label, err)
			return ""
		}
		return string(data)
	}

	got1 := readMsg(c1, "client 1")
	got2 := readMsg(c2, "client 2")

	if got1 != want {
		t.Errorf("client 1: got %q, want %q", got1, want)
	}
	if got2 != want {
		t.Errorf("client 2: got %q, want %q", got2, want)
	}

	// Close stdin to stop the echo subprocess.
	proc.stdin.Close()
}

// TestWaitLoopMarksSessionDead verifies that when the subprocess exits,
// the store session is marked Alive=false with an ExitCode.
func TestWaitLoopMarksSessionDead(t *testing.T) {
	s := newTestStore("sess-3")
	m := New(s)

	launchHelper(t, m, "sess-3", "emit-and-exit")

	waitFor(t, 5*time.Second, "session alive=false", func() bool {
		sess, _ := s.Get("sess-3")
		return !sess.Alive
	})

	sess, _ := s.Get("sess-3")
	if sess.ExitCode == nil {
		t.Error("ExitCode should be set after exit")
	} else if *sess.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", *sess.ExitCode)
	}
	if sess.ExitedAt == "" {
		t.Error("ExitedAt should be set after exit")
	}
}

// TestHandleWebSocketUnknownSession verifies a 404 for sessions not in the manager.
func TestHandleWebSocketUnknownSession(t *testing.T) {
	m := New(store.New())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.HandleWebSocket(w, r, "no-such-session")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestShutdown verifies that Shutdown closes all running subprocesses and
// the store transitions the sessions to dead.
func TestShutdown(t *testing.T) {
	s := newTestStore("sess-4")
	m := New(s)

	launchHelper(t, m, "sess-4", "emit-session-ready")

	// Wait for the subprocess to be fully running (subtitle updated).
	waitFor(t, 5*time.Second, "subtitle set", func() bool {
		sess, _ := s.Get("sess-4")
		return sess.Subtitle == "test-model"
	})

	m.Shutdown(5 * time.Second)

	// After Shutdown, the session should be dead.
	waitFor(t, 5*time.Second, "session dead after Shutdown", func() bool {
		sess, _ := s.Get("sess-4")
		return !sess.Alive
	})
}
