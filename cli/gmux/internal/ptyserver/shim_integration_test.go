package ptyserver

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestShimAttributesOnReadOpen checks the /resume-select path: the agent
// opens an existing session file for READ (no write), and the runner still
// attributes it. Mirrors pi binding to a conversation without writing until
// the next message.
func TestShimAttributesOnReadOpen(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	dir := t.TempDir()
	sessFile := filepath.Join(dir, "2026-06-16_sess-resumed.jsonl")
	sockPath := filepath.Join(dir, "test.sock")

	// Pre-create the file on disk, then have the agent ONLY read it (no
	// write) — exactly what pi does when you select a session to resume.
	if err := os.WriteFile(sessFile, []byte(`{"type":"session","id":"sess-resumed"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed session file: %v", err)
	}
	script := `const fs=require("fs");` +
		`const fd=fs.openSync(process.env.SESS,"r");fs.readFileSync(fd);fs.closeSync(fd);` +
		`setTimeout(()=>{},800);`

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", script},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
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
			t.Fatalf("shim never attributed on read-open; SessionFile=%q", st.SessionFile)
		case <-time.After(50 * time.Millisecond):
			if st.SessionFile == sessFile {
				return // success
			}
		}
	}
}

// TestShimAttributesOnAsyncReadOpen is the same bind-on-read check but via
// the promise API (fs.promises.open) instead of openSync, proving the shim
// isn't coupled to a specific (sync vs async) read method.
func TestShimAttributesOnAsyncReadOpen(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	dir := t.TempDir()
	sessFile := filepath.Join(dir, "2026-06-16_sess-async.jsonl")
	sockPath := filepath.Join(dir, "test.sock")

	if err := os.WriteFile(sessFile, []byte(`{"type":"session","id":"sess-async"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed session file: %v", err)
	}
	// Open for read via the promise API only — no openSync, no write.
	script := `const fs=require("fs");` +
		`fs.promises.open(process.env.SESS,"r").then(h=>h.close());` +
		`setTimeout(()=>{},800);`

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", script},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
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
			t.Fatalf("shim never attributed on async read-open; SessionFile=%q", st.SessionFile)
		case <-time.After(50 * time.Millisecond):
			if st.SessionFile == sessFile {
				return
			}
		}
	}
}

// TestShimBulkReadScanDoesNotAttribute simulates a /resume picker: the agent
// reads MANY session files in a burst (via readFileSync). The runner's read
// debouncer must treat this as a scan and attribute none of them.
func TestShimBulkReadScanDoesNotAttribute(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	var files []string
	for _, n := range []string{"a", "b", "c", "d"} {
		f := filepath.Join(dir, "2026-06-16_sess-"+n+".jsonl")
		if err := os.WriteFile(f, []byte(`{"type":"session","id":"`+n+`"}`+"\n"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		files = append(files, f)
	}
	// Open every file for read in a tight burst (no write), like a picker that
	// scans the list via file handles. The debouncer must see >1 distinct
	// file and attribute none.
	script := `const fs=require("fs");` +
		`for (const f of process.env.FILES.split(",")) { const fd=fs.openSync(f,"r"); fs.closeSync(fd); }` +
		`setTimeout(()=>{},800);`

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", script},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
		Env:        []string{"FILES=" + strings.Join(files, ",")},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Give the burst + debounce window time to resolve, then assert nothing
	// was attributed.
	time.Sleep(700 * time.Millisecond)
	if st.SessionFile != "" {
		t.Fatalf("bulk read scan wrongly attributed %q", st.SessionFile)
	}
}

// TestShimReannounceOnReconnect verifies the runner replays its current
// shim state (shim + session_file) to a freshly-connected /events
// subscriber, so a restarted daemon re-learns attribution without any
// persisted state.
func TestShimReannounceOnReconnect(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "2026-06-16_sess-reconnect.jsonl")
	sockPath := filepath.Join(dir, "test.sock")

	// Write the session file, then idle long enough for a second
	// subscriber to connect and read the replay.
	script := `const fs=require("fs");` +
		`fs.appendFileSync(process.env.SESS, JSON.stringify({type:"session",id:"sess-reconnect"})+"\n");` +
		`setTimeout(()=>{},3000);`

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", script},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
		Env:        []string{"SESS=" + sessFile},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Wait until the runner has recorded the session file.
	deadline := time.After(5 * time.Second)
	for st.SessionFile != sessFile {
		select {
		case <-deadline:
			t.Fatalf("runner never recorded session file; got %q", st.SessionFile)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Now connect a NEW /events subscriber (simulating a daemon reconnect)
	// and read the replayed snapshot.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/events", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("connect /events: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var sawShim, sawFile bool
	for sc.Scan() {
		line := sc.Text()
		if line == "event: shim" {
			sawShim = true
		}
		if strings.Contains(line, sessFile) {
			sawFile = true
		}
		if sawShim && sawFile {
			break
		}
	}
	if !sawShim {
		t.Error("reconnecting subscriber did not receive replayed shim event")
	}
	if !sawFile {
		t.Error("reconnecting subscriber did not receive replayed session_file")
	}
}
