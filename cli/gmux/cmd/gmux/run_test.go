package main

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
)

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

// TestFinalizeSessionStateClosesLifetimeTurnBeforeExit pins the
// event ordering the daemon's wait machinery depends on (ADR 0023): a
// lifetime-turn session's exit must emit the turn-close status and the
// unread flag BEFORE the exit event. A subscriber that resolves on the
// first terminal signal it sees must observe the closed turn, and the
// store's exit handling must persist the final Status — emitting the
// exit first would resolve waits as "died" and persist a stale
// mid-turn Working=true.
func TestFinalizeSessionStateClosesLifetimeTurnBeforeExit(t *testing.T) {
	st := session.New(session.Config{ID: "sess-finalize", Adapter: "shell"})
	st.SetStatus(&adapter.Status{Working: true}) // launch state, pre-subscription
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	finalizeSessionState(st, true, 3)

	got := collectEvents(t, ch, 3)
	wantTypes := []string{"status", "meta", "exit"}
	for i, ev := range got {
		if ev.Type != wantTypes[i] {
			t.Fatalf("event %d = %q, want %q (order: %v)", i, ev.Type, wantTypes[i], got)
		}
	}
	status, ok := got[0].Data.(*adapter.Status)
	if !ok || status == nil || status.Working {
		t.Errorf("turn-close status = %#v, want Working=false", got[0].Data)
	}
	if !status.Error {
		t.Error("Error = false for exit code 3, want true (failed one-shot shows the error dot)")
	}
	if !st.UnreadSnapshot() {
		t.Error("unread not set; a completed lifetime turn is 'waiting on you'")
	}
}

// TestFinalizeSessionStateLeavesUpgradedTurnAlone: a session whose
// turns are mark-delimited (lifetimeTurnOpen == false) must get only
// the exit event — its last mark-derived Status is the truth. A shell
// killed mid-command stays Working=true and resolves as "died"; one
// that exited at its prompt already reads idle.
func TestFinalizeSessionStateLeavesUpgradedTurnAlone(t *testing.T) {
	st := session.New(session.Config{ID: "sess-upgraded", Adapter: "shell"})
	st.SetStatus(&adapter.Status{Working: true}) // mid-command
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	finalizeSessionState(st, false, 0)

	got := collectEvents(t, ch, 1)
	if got[0].Type != "exit" {
		t.Fatalf("first event = %q, want exit (and nothing before it)", got[0].Type)
	}
	if s := st.StatusSnapshot(); s == nil || !s.Working {
		t.Errorf("Status = %+v, want the mark-derived Working=true preserved", s)
	}
	if st.UnreadSnapshot() {
		t.Error("unread set on a mid-turn death; nothing completed")
	}
}
