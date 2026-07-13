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

// plainAdapter is a minimal adapter that does NOT implement
// adapter.PromptSignaler, standing in for any non-shell adapter.
type plainAdapter struct{}

func (plainAdapter) Name() string                      { return "plain" }
func (plainAdapter) Discover() bool                    { return true }
func (plainAdapter) Match(_ []string) bool             { return true }
func (plainAdapter) Env(_ adapter.EnvContext) []string { return nil }

// TestPromptMarksIgnoredForNonSignalerAdapters guards the capability
// gate: OSC 133 marks in the output of an adapter that doesn't opt in
// via PromptSignaler must not touch Status (agent adapters own their
// Status via hooks; a stray mark must not fight them).
func TestPromptMarksIgnoredForNonSignalerAdapters(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "sess-nomarks", Adapter: "plain"})

	srv, err := New(Config{
		Command:    []string{"bash", "-c", `printf '\e]133;C\a\e]133;A\a'`},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    plainAdapter{},
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
		t.Errorf("Status = %+v, want nil for a non-PromptSignaler adapter", got)
	}
}
