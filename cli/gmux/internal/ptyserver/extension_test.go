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

func TestAgentHookDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},  // unset / empty → hook enabled
		{"0", false}, // explicit off-switch → enabled
		{"1", true},  // the documented way to disable
		{"true", true},
		{"anything", true},
	}
	for _, tc := range cases {
		t.Setenv("GMUX_NO_AGENT_HOOK", tc.val)
		if got := agentHookDisabled(); got != tc.want {
			t.Errorf("GMUX_NO_AGENT_HOOK=%q: agentHookDisabled()=%v, want %v", tc.val, got, tc.want)
		}
	}
}

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

// TestReconnectReplaysConversationRef checks that a newly-connected /events
// subscriber (a reconnecting daemon) is replayed the bound conversation file, so
// attribution survives a daemon restart without persisted state.
func TestReconnectReplaysConversationRef(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	sessFile := filepath.Join(dir, "2026-06-19_sess-reconnect.jsonl")

	st := session.New(session.Config{ID: "s1", Adapter: "pi", SocketPath: sockPath})
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
	for st.ConversationRefSnapshot() != sessFile {
		select {
		case <-deadline:
			t.Fatalf("runner never recorded conversation file; got %q", st.ConversationRefSnapshot())
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
			return // success: conversation_file replayed
		}
	}
	t.Error("reconnecting subscriber did not receive replayed conversation_file")
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

	st := session.New(session.Config{ID: "s1", Adapter: "pi", SocketPath: sockPath})
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
		for st.ConversationRefSnapshot() != want {
			select {
			case <-deadline:
				t.Fatalf("runner did not bind to %q; got %q", want, st.ConversationRefSnapshot())
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

// TestTurnEventDrivesState checks the extension turn path: a "turn" start goes
// working, and a "turn" end with outcome "completed" goes idle + unread and
// applies the title — no file parsing. The outcome→state policy lives in the
// runner (applyTurnEnd), so this is also its mapping test.
func TestTurnEventDrivesState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	st := session.New(session.Config{ID: "s1", Adapter: "pi", SocketPath: sockPath})
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

	post(`{"op":"turn","phase":"start"}`)
	deadline := time.After(2 * time.Second)
	for s := st.StatusSnapshot(); s == nil || !s.Working; s = st.StatusSnapshot() {
		select {
		case <-deadline:
			t.Fatalf("status never went working; got %+v", st.StatusSnapshot())
		case <-time.After(20 * time.Millisecond):
		}
	}

	post(`{"op":"turn","phase":"end","outcome":"completed","title":"my chat"}`)
	deadline = time.After(2 * time.Second)
	for {
		s := st.StatusSnapshot()
		if s != nil && !s.Working && st.Title() == "my chat" && st.UnreadSnapshot() {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("status/title not applied; status=%+v title=%q unread=%v", st.StatusSnapshot(), st.Title(), st.UnreadSnapshot())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestSessionSlugFromExplicitSourceOnly pins, through the real /hook/event
// handler, that the runner sets the slug ONLY from an explicit title-derived
// source and never synthesizes one from the adapter session id: ev.ID is a
// UUID for every real adapter, and slugifying it produces an unreadable
// full-UUID URL that also defeats the web's short id.slice(0,8) fallback
// (the pre-title-window regression behind #360's follow-up). With no slug
// source the runner leaves Slug empty for the web layer to fill.
func TestSessionSlugFromExplicitSourceOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	newSrv := func(t *testing.T) (*Server, *session.State, string) {
		t.Helper()
		dir := t.TempDir()
		sockPath := filepath.Join(dir, "test.sock")
		st := session.New(session.Config{ID: "s1", Adapter: "codex", SocketPath: sockPath})
		srv, err := New(Config{
			Command:    []string{node, "-e", "setTimeout(()=>{},2000)"},
			Cwd:        dir,
			Listener:   mustBindSocket(t, sockPath),
			SocketPath: sockPath,
			Adapter:    adapters.NewCodex(),
			State:      st,
		})
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		t.Cleanup(srv.Shutdown)
		return srv, st, sockPath
	}

	// A real title-derived slug is slugified and recorded.
	t.Run("explicit-slug", func(t *testing.T) {
		_, st, sockPath := newSrv(t)
		postSessionEvent(t, sockPath,
			`{"op":"session","path":"/x.jsonl","id":"019cfb54-dead-beef","slug":"fix the auth bug"}`)
		deadline := time.After(2 * time.Second)
		for st.SlugSnapshot() != "fix-the-auth-bug" {
			select {
			case <-deadline:
				t.Fatalf("slug = %q, want %q", st.SlugSnapshot(), "fix-the-auth-bug")
			case <-time.After(20 * time.Millisecond):
			}
		}
	})

	// A pre-title bind (only an id, no slug) must leave the slug EMPTY so the
	// web fallback applies. Poll for a settle window to catch a synthesized
	// slug that arrives slightly after the POST returns.
	t.Run("no-slug-source-stays-empty", func(t *testing.T) {
		_, st, sockPath := newSrv(t)
		postSessionEvent(t, sockPath,
			`{"op":"session","path":"/y.jsonl","id":"019f2c75-5279-7012-b054-ce2a71441a4e"}`)
		deadline := time.After(500 * time.Millisecond)
		for {
			if got := st.SlugSnapshot(); got != "" {
				t.Fatalf("slug = %q, want empty (no title source → web owns the fallback)", got)
			}
			select {
			case <-deadline:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})

	// A bind is authoritative: switching from a titled conversation to a
	// fresh untitled one (pi re-binds through the same runner on
	// new/switch/resume/fork) must CLEAR the old slug, not keep serving it.
	t.Run("no-slug-source-clears-prior-slug", func(t *testing.T) {
		_, st, sockPath := newSrv(t)
		postSessionEvent(t, sockPath,
			`{"op":"session","path":"/a.jsonl","id":"id-a","slug":"fix the auth bug"}`)
		deadline := time.After(2 * time.Second)
		for st.SlugSnapshot() != "fix-the-auth-bug" {
			select {
			case <-deadline:
				t.Fatalf("setup: slug = %q, want %q", st.SlugSnapshot(), "fix-the-auth-bug")
			case <-time.After(20 * time.Millisecond):
			}
		}
		postSessionEvent(t, sockPath,
			`{"op":"session","path":"/b.jsonl","id":"019f2c75-5279-7012-b054-ce2a71441a4e","reason":"new"}`)
		deadline = time.After(2 * time.Second)
		for st.SlugSnapshot() != "" {
			select {
			case <-deadline:
				t.Fatalf("slug = %q, want cleared after untitled re-bind", st.SlugSnapshot())
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
}

// TestApplyTurnEnd pins the outcome→sidebar-state policy directly (no node).
func TestApplyTurnEnd(t *testing.T) {
	cases := []struct {
		outcome    string
		wantUnread bool
		wantError  bool
	}{
		{"completed", true, false},
		{"aborted", false, false},
		{"error", false, true},
	}
	for _, tc := range cases {
		st := session.New(session.Config{ID: "s1", Adapter: "pi"})
		srv := &Server{state: st}
		srv.applyTurnEnd(tc.outcome, "")
		status := st.StatusSnapshot()
		if status == nil || status.Working {
			t.Errorf("%s: expected idle, got %+v", tc.outcome, status)
		}
		if st.UnreadSnapshot() != tc.wantUnread {
			t.Errorf("%s: unread=%v want %v", tc.outcome, st.UnreadSnapshot(), tc.wantUnread)
		}
		if status != nil && status.Error != tc.wantError {
			t.Errorf("%s: error=%v want %v", tc.outcome, status.Error, tc.wantError)
		}
	}
}
