package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseClaudeHookCLI(t *testing.T) {
	cmd, err := parseCLI([]string{"__claude-hook"})
	if err != nil || cmd.mode != modeClaudeHook {
		t.Fatalf("want modeClaudeHook, got %+v err=%v", cmd, err)
	}
}

func TestRunClaudeHookInertWithoutSocket(t *testing.T) {
	// No socket: must drain stdin and return without panicking.
	runClaudeHook(strings.NewReader(`{"hook_event_name":"UserPromptSubmit"}`), "")
}

func TestRunClaudeHookPosts(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var paths []string
	done := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(200)
	})}
	go func() { srv.Serve(ln); close(done) }()
	defer srv.Close()

	runClaudeHook(strings.NewReader(`{"hook_event_name":"UserPromptSubmit"}`), sock)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/hook/event" {
		t.Fatalf("want one POST to /hook/event, got %v", paths)
	}
}

func TestClaudeHookExitsZero(t *testing.T) {
	// Redirect stdin to empty so claudeHook doesn't block; it must exit 0 and
	// (implicitly) write nothing to stdout.
	old := os.Stdin
	r, _, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()
	r.Close() // EOF immediately
	if code := claudeHook(); code != 0 {
		t.Fatalf("claudeHook must exit 0, got %d", code)
	}
}
