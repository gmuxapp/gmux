package ptyserver

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// collectStatusEvents drains status events from ch until want
// transitions arrived or the deadline passed, returning the observed
// Working sequence.
func collectStatusEvents(t *testing.T, ch chan session.Event, want int, deadline time.Duration) []bool {
	t.Helper()
	var got []bool
	timeout := time.After(deadline)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			if ev.Type != "status" {
				continue
			}
			status, ok := ev.Data.(*adapter.Status)
			if !ok || status == nil {
				t.Fatalf("status event with unexpected payload %#v", ev.Data)
			}
			got = append(got, status.Working)
		case <-timeout:
			return got
		}
	}
	return got
}

// TestShellPromptMarksDriveStatus is the runner-side integration test
// for issue #373: a shell session whose output carries OSC 133 prompt
// marks gets busy/idle Status transitions derived by the runner. The
// fast second phase prints the command-start and prompt-start marks in
// one burst (a single PTY read), pinning the property that the full
// working→idle pulse is emitted as two distinct status events — that
// pulse is what the daemon's send --wait keys on.
func TestShellPromptMarksDriveStatus(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "sess-promptmarks", Adapter: "shell"})

	// Subscribe before launch so no transition can be missed.
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	srv, err := New(Config{
		Command: []string{"bash", "-c",
			// Phase 1: first prompt (idle). Phase 2, after the coalesce
			// window has flushed: a fast command cycle whose C and D+A
			// marks land in one chunk.
			`printf '\e]133;A\a$ '; sleep 0.3; printf '\e]133;C\adone\n\e]133;D;0\a\e]133;A\a$ '; sleep 0.2`},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewShell(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	got := collectStatusEvents(t, ch, 3, 5*time.Second)
	want := []bool{false, true, false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status transitions = %v, want %v", got, want)
	}
}

// hookedAdapter is a minimal hook-driven adapter (SessionExtender),
// standing in for the agent adapters whose turn state is owned by
// their hooks.
type hookedAdapter struct{}

func (hookedAdapter) Name() string                      { return "hooked" }
func (hookedAdapter) Discover() bool                    { return true }
func (hookedAdapter) Match(_ []string) bool             { return true }
func (hookedAdapter) Env(_ adapter.EnvContext) []string { return nil }
func (hookedAdapter) ExtendCommand(args []string, _ string) []string {
	return args
}

// TestPromptMarksIgnoredForHookDrivenAdapters guards the turn-source
// split: OSC 133 marks in the output of a hook-driven (agent) adapter
// must not touch Status — the agent's hook is the sole owner of its
// turn state, and a stray mark (e.g. printed by a tool the agent ran)
// must not fight it.
func TestPromptMarksIgnoredForHookDrivenAdapters(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "sess-nomarks", Adapter: "hooked"})

	srv, err := New(Config{
		Command:    []string{"bash", "-c", `printf '\e]133;C\a\e]133;A\a'`},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    hookedAdapter{},
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	select {
	case <-srv.PTYDone():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit in time")
	}
	if got := st.StatusSnapshot(); got != nil {
		t.Errorf("Status = %+v, want nil for a hook-driven adapter", got)
	}
	if srv.LifetimeTurnOpen() {
		t.Error("LifetimeTurnOpen() = true for a hook-driven session; the default turn model must not apply")
	}
}

// TestPromptCycleSetsUnreadOnCommandCompletion pins the "waiting on
// you" parity rule: a genuine working→idle prompt cycle flags the
// session unread (like an agent's completed turn), while the shell's
// initial prompt — also an idle transition — does not.
func TestPromptCycleSetsUnreadOnCommandCompletion(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "sess-unread", Adapter: "shell"})

	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	srv, err := New(Config{
		Command: []string{"bash", "-c",
			// First prompt, pause (flush), then a full command cycle.
			`printf '\e]133;A\a$ '; sleep 0.3; printf '\e]133;C\adone\n\e]133;D;0\a\e]133;A\a$ '`},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewShell(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// After the first idle transition (initial prompt): no unread.
	if got := collectStatusEvents(t, ch, 1, 3*time.Second); len(got) != 1 || got[0] {
		t.Fatalf("first transition = %v, want [false]", got)
	}
	if st.UnreadSnapshot() {
		t.Error("unread = true after initial prompt; want false (no command completed yet)")
	}

	// After the full working→idle cycle: unread.
	if got := collectStatusEvents(t, ch, 2, 3*time.Second); len(got) != 2 || got[0] != true || got[1] != false {
		t.Fatalf("cycle transitions = %v, want [true false]", got)
	}
	if !st.UnreadSnapshot() {
		t.Error("unread = false after a completed prompt cycle; want true (waiting on you)")
	}
	if srv.LifetimeTurnOpen() {
		t.Error("LifetimeTurnOpen() = true after prompt marks were observed; session should be upgraded")
	}
}

// TestLifetimeTurnStaysOpenWithoutMarks pins the default turn model's
// upgrade signal: a session whose output never carries prompt marks
// keeps its lifetime turn open (run.go closes it at exit — that's what
// resolves waits on one-shot commands as "idle").
func TestLifetimeTurnStaysOpenWithoutMarks(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "sess-lifetime", Adapter: "shell"})

	srv, err := New(Config{
		Command:    []string{"bash", "-c", `echo plain output, no marks`},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewShell(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	select {
	case <-srv.PTYDone():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit in time")
	}
	if !srv.LifetimeTurnOpen() {
		t.Error("LifetimeTurnOpen() = false for a markless session; want true (exit closes the turn)")
	}
	if st.UnreadSnapshot() {
		t.Error("unread = true without any completed turn; want false (exit-close is run.go's job)")
	}
}
