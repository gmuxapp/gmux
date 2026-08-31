package main

import (
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/ptyserver"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

func TestInheritLaunchParentOnlyForFreshUnboundLaunch(t *testing.T) {
	getenv := func(name string) string {
		if name != "GMUX_SESSION_ID" {
			t.Fatalf("unexpected env lookup %q", name)
		}
		return "parent"
	}
	if got := inheritLaunchParent(runDirectives{}, getenv).ParentSessionID; got != "parent" {
		t.Fatalf("fresh nested launch parent=%q, want parent", got)
	}
	if got := inheritLaunchParent(runDirectives{ParentSessionID: "explicit"}, getenv).ParentSessionID; got != "explicit" {
		t.Fatalf("explicit parent overwritten: %q", got)
	}
	if got := inheritLaunchParent(runDirectives{ResumeID: "existing"}, getenv).ParentSessionID; got != "" {
		t.Fatalf("resume acquired parent %q", got)
	}
}

func TestExplicitResumeIDNeverFallsBackToPhantomSession(t *testing.T) {
	if mayRetrySessionID("1juyvpd8", ptyserver.ErrSocketInUse) {
		t.Fatal("explicit resume collision would mint an unrelated session id")
	}
	if !mayRetrySessionID("", ptyserver.ErrSocketInUse) {
		t.Fatal("ordinary generated-id collision should remain retryable")
	}
	if mayRetrySessionID("", errors.New("bind failed")) {
		t.Fatal("non-collision bind error became retryable")
	}
}

// collectEvents drains n events from ch (or times out), returning the
// event types in arrival order plus the payloads for inspection.
func collectEvents(t *testing.T, ch chan session.Event, n int) []session.Event {
	t.Helper()
	var got []session.Event
	timeout := time.After(2 * time.Second)
	for len(got) < n {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out after %d/%d events: %+v", len(got), n, got)
		}
	}
	return got
}

// TestFinalizeSessionStateClosesLifetimeTurnBeforeExit pins the atomic event
// shape the daemon's wait machinery depends on (ADR 0023): lifetime status,
// exit, and unread generation are one terminal event.
func TestFinalizeSessionStateClosesLifetimeTurnBeforeExit(t *testing.T) {
	st := session.New(session.Config{ID: "1uo92yti", Adapter: "shell"})
	st.SetStatus(&adapter.Status{Active: true}) // launch state, pre-subscription
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	finalizeSessionState(st, true, 3)

	got := collectEvents(t, ch, 1)
	if got[0].Type != "exit" {
		t.Fatalf("events = %+v, want one fused lifetime exit", got)
	}
	raw, err := json.Marshal(got[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	var edge struct {
		Active      bool   `json:"active"`
		Error       bool   `json:"error"`
		Unread      bool   `json:"unread"`
		UnreadToken string `json:"unread_token"`
	}
	if err := json.Unmarshal(raw, &edge); err != nil {
		t.Fatal(err)
	}
	if edge.Active || !edge.Error || !edge.Unread || edge.UnreadToken == "" {
		t.Fatalf("fused completion edge = %+v", edge)
	}
	if !st.UnreadSnapshot() {
		t.Error("unread not set; a completed lifetime turn is 'waiting on you'")
	}
}

// TestFinalizeSessionStateOSCCommandCrashSetsUnread reproduces an OSC C
// command-start followed by child exit before D. The mark-derived Active bit
// remains authoritative (wait resolves as died), while process completion is
// still universally unread and is fused into the exit event.
func TestFinalizeSessionStateOSCCommandCrashSetsUnread(t *testing.T) {
	st := session.New(session.Config{ID: "10gxyrcs", Adapter: "shell"})
	marks := adapter.NewPromptMarkTracker(func(active bool) {
		st.SetStatus(&adapter.Status{Active: active})
	})
	marks.Feed([]byte("\x1b]133;C\a"))
	if !marks.SawMark() {
		t.Fatal("OSC C did not upgrade the terminal turn source")
	}
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	finalizeSessionState(st, false, 7)

	got := collectEvents(t, ch, 1)
	if got[0].Type != "exit" {
		t.Fatalf("events = %+v, want fused unread exit", got)
	}
	data, ok := got[0].Data.(map[string]any)
	if !ok || data["unread"] != true || data["unread_token"] == "" {
		t.Fatalf("exit payload = %#v, want unread result token", got[0].Data)
	}
	if s := st.StatusSnapshot(); s == nil || !s.Active {
		t.Errorf("Status = %+v, want OSC-derived Active=true preserved", s)
	}
	if !st.UnreadSnapshot() {
		t.Error("OSC C → crash completion did not set unread")
	}
}

func TestRunnerOSCCommandCrashBeforeDCompletesUnread(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	st := session.New(session.Config{ID: "10gxyrcs", Adapter: "shell", SocketPath: socketPath})
	srv, err := ptyserver.New(ptyserver.Config{
		Command: []string{"/bin/sh", "-c", `printf '\033]133;C\a'; exit 7`},
		Cwd:     "/tmp", Listener: ln, SocketPath: socketPath, Adapter: adapters.NewShell(), State: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	select {
	case <-srv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("runner child did not exit")
	}
	select {
	case <-srv.PTYDone():
	case <-time.After(5 * time.Second):
		t.Fatal("runner PTY did not drain")
	}
	if srv.LifetimeTurnOpen() {
		t.Fatal("OSC C did not replace the lifetime turn")
	}
	finalizeSessionState(st, srv.LifetimeTurnOpen(), srv.ExitCode())
	if status := st.StatusSnapshot(); status == nil || !status.Active {
		t.Fatalf("crash status = %+v, want OSC-derived active retained", status)
	}
	if !st.UnreadSnapshot() || st.ExitCode == nil || *st.ExitCode != 7 {
		t.Fatalf("crash state unread=%v exit=%v", st.UnreadSnapshot(), st.ExitCode)
	}
}

func TestFinalizeSessionStateInterruptedExitDoesNotCreateUnread(t *testing.T) {
	st := session.New(session.Config{ID: "10gxyrcs", Adapter: "pi"})
	st.SetStatus(&adapter.Status{Interrupted: true})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	finalizeSessionState(st, false, 130)
	got := collectEvents(t, ch, 1)
	if got[0].Type != "exit" || st.UnreadSnapshot() {
		t.Fatalf("interrupted exit = events %+v unread=%v", got, st.UnreadSnapshot())
	}
}
