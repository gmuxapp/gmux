package main

import (
	"io"
	"net"
	"net/http"
	"path/filepath"
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
