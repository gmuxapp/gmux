package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCodexHookCLI(t *testing.T) {
	cmd, err := parseCLI([]string{"__codex-hook", "SessionStart"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cmd.mode != modeCodexHook || cmd.codexHookEvent != "SessionStart" {
		t.Errorf("got mode=%v event=%q", cmd.mode, cmd.codexHookEvent)
	}

	if _, err := parseCLI([]string{"__codex-hook"}); err == nil {
		t.Error("expected error for missing event name")
	}
	if _, err := parseCLI([]string{"__codex-hook", "a", "b"}); err == nil {
		t.Error("expected error for too many args")
	}
}

func TestPostCodexHookEvent(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var (
		mu   sync.Mutex
		got  []byte
		path string
	)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path = r.URL.Path
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	postCodexHookEvent(sockPath, []byte(`{"op":"turn","phase":"start"}`))

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		body, p := string(got), path
		mu.Unlock()
		if body == `{"op":"turn","phase":"start"}` && p == "/hook/event" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("runner did not receive event; path=%q body=%q", p, body)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// codexHookSink starts a unix-socket HTTP server that records the bodies POSTed
// to /hook/event, and returns its socket path plus an accessor for the bodies.
func codexHookSink(t *testing.T) (sockPath string, bodies func() []string) {
	t.Helper()
	sockPath = filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+" "+string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); ln.Close() })
	return sockPath, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestRunCodexHookInertWithoutSocket pins the security-relevant property: with
// no GMUX_SESSION_SOCK (a plain `codex` run, not launched by gmux) the injected
// hook posts nothing and still emits the obligatory "{}" so codex is unaffected.
func TestRunCodexHookInertWithoutSocket(t *testing.T) {
	sock, bodies := codexHookSink(t)
	var out bytes.Buffer
	in := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"x"}`)

	runCodexHook("UserPromptSubmit", in, &out, "") // empty sock

	if strings.TrimSpace(out.String()) != "{}" {
		t.Errorf("stdout = %q, want \"{}\"", out.String())
	}
	// Give any (erroneous) post a moment to land, then assert none did.
	time.Sleep(50 * time.Millisecond)
	if n := len(bodies()); n != 0 {
		t.Errorf("posted %d events with no socket; want 0 (sink %s)", n, sock)
	}
}

// TestRunCodexHookPostsTurnStart drives the happy path: a UserPromptSubmit with
// a live socket posts exactly the turn-start event and emits "{}".
func TestRunCodexHookPostsTurnStart(t *testing.T) {
	sock, bodies := codexHookSink(t)
	var out bytes.Buffer

	runCodexHook("UserPromptSubmit", strings.NewReader("{}"), &out, sock)

	if strings.TrimSpace(out.String()) != "{}" {
		t.Errorf("stdout = %q, want \"{}\"", out.String())
	}
	deadline := time.After(2 * time.Second)
	for {
		b := bodies()
		if len(b) == 1 && b[0] == `/hook/event {"op":"turn","phase":"start"}` {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("did not receive turn-start; got %v", b)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestPostCodexHookEventNoListener proves the post is best-effort: a dead
// socket path must not panic or hang (the hook never breaks codex).
func TestPostCodexHookEventNoListener(t *testing.T) {
	done := make(chan struct{})
	go func() {
		postCodexHookEvent(filepath.Join(t.TempDir(), "absent.sock"), []byte(`{}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("postCodexHookEvent hung on a dead socket")
	}
}
