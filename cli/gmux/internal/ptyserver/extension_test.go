package ptyserver

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// postSessionEvent posts an extension event to the runner's /hook/event.
func postSessionEvent(t *testing.T, sockPath, body string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", sockPath)
	}}}
	resp, err := c.Post("http://unix/hook/event", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	resp.Body.Close()
}

// TestReconnectReplaysSessionFile checks that a newly-connected /events
// subscriber (a reconnecting daemon) is replayed the bound session file, so
// attribution survives a daemon restart without persisted state.
func TestReconnectReplaysSessionFile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	sessFile := filepath.Join(dir, "2026-06-19_sess-reconnect.jsonl")

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", "setTimeout(()=>{},3000)"},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	postSessionEvent(t, sockPath, `{"op":"session","path":`+strconv.Quote(sessFile)+`}`)
	deadline := time.After(5 * time.Second)
	for st.SessionFile != sessFile {
		select {
		case <-deadline:
			t.Fatalf("runner never recorded session file; got %q", st.SessionFile)
		case <-time.After(20 * time.Millisecond):
		}
	}

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
	for sc.Scan() {
		if strings.Contains(sc.Text(), sessFile) {
			return // success: session_file replayed
		}
	}
	t.Error("reconnecting subscriber did not receive replayed session_file")
}
func TestSessionEventIsAuthoritative(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	fileA := filepath.Join(dir, "2026-06-19_sess-A.jsonl")
	fileB := filepath.Join(dir, "2026-06-19_sess-B.jsonl")
	for _, f := range []string{fileA, fileB} {
		if err := os.WriteFile(f, []byte(`{"type":"session"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", "setTimeout(()=>{},2000)"},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	post := func(path string) {
		body := `{"op":"session","path":` + strconv.Quote(path) + `,"reason":"resume"}`
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		}}}
		resp, err := c.Post("http://unix/hook/event", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post session event: %v", err)
		}
		resp.Body.Close()
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for st.SessionFile != want {
			select {
			case <-deadline:
				t.Fatalf("runner did not bind to %q; got %q", want, st.SessionFile)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	post(fileA)
	waitFor(fileA)
	// Rebind to a different file (cache-served /resume-select).
	post(fileB)
	waitFor(fileB)
}

// TestStatusEventDrivesState checks the extension status path: a "status"
// event sets working/unread/title on session state directly, with no file
// parsing.
func TestStatusEventDrivesState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	st := session.New(session.Config{ID: "s1", Kind: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{node, "-e", "setTimeout(()=>{},2000)"},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	post := func(body string) {
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		}}}
		resp, err := c.Post("http://unix/hook/event", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
	}

	post(`{"op":"status","working":true}`)
	deadline := time.After(2 * time.Second)
	for st.Status == nil || !st.Status.Working {
		select {
		case <-deadline:
			t.Fatalf("status never went working; got %+v", st.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}

	post(`{"op":"status","working":false,"unread":true,"title":"my chat"}`)
	deadline = time.After(2 * time.Second)
	for st.Status == nil || st.Status.Working || st.Title() != "my chat" || !st.Unread {
		select {
		case <-deadline:
			t.Fatalf("status/title not applied; status=%+v title=%q unread=%v", st.Status, st.Title(), st.Unread)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
