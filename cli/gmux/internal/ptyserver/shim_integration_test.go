package ptyserver

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// TestShimReportsSessionFile is an end-to-end check of the agent-shim path:
// the runner injects the preload for a SessionShimmer adapter (pi), a real
// node process writes a *.jsonl session file, the shim posts it to the
// runner socket, and the runner records it on session state.
func TestShimReportsSessionFile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	dir := t.TempDir()
	sessFile := filepath.Join(dir, "2026-06-16_sess-xyz.jsonl")
	sockPath := filepath.Join(dir, "test.sock")

	// Minimal agent: write a session header to a .jsonl, then idle briefly
	// so the runner stays up long enough to receive the shim POST.
	script := `const fs=require("fs");` +
		`fs.appendFileSync(process.env.SESS, JSON.stringify({type:"session",id:"sess-xyz"})+"\n");` +
		`setTimeout(()=>{},800);`

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})

	srv, err := New(Config{
		Command:    []string{node, "-e", script},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(), // implements SessionShimmer
		State:      st,
		Env:        []string{"SESS=" + sessFile},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("shim never reported session file; SessionFile=%q", st.SessionFile)
		case <-time.After(50 * time.Millisecond):
			if st.SessionFile == sessFile {
				return // success
			}
		}
	}
}
