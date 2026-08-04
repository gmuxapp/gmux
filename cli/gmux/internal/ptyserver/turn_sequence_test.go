package ptyserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// postHook feeds one tool-neutral hook body through the real runner handler,
// exactly as an agent's hook would over $GMUX_SESSION_SOCK.
func postHook(t *testing.T, srv *Server, body []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleHookEvent(rec, httptest.NewRequest("POST", "http://unix/hook/event", bytes.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("hook event %s → %d", body, rec.Code)
	}
}

// claudeEvent runs Claude's real hook translator (the stateless
// `gmux __claude-hook` payload → tool-neutral bodies step) and posts every
// body it produces, so these tests pin the whole sequence rather than the
// runner's policy in isolation.
func claudeEvent(t *testing.T, srv *Server, name string) {
	t.Helper()
	in, err := json.Marshal(map[string]string{"hook_event_name": name})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range adapters.ClaudeHookBodies(in) {
		postHook(t, srv, b)
	}
}

// TestClaudeTurnSequences pins the two Claude flows that differ only in
// whether Stop fires. SessionEnd is unconditional on exit, so without the
// runner's open-turn gate the normal flow would end completed and then
// immediately overwrite that durable state with an interruption.
func TestClaudeTurnSequences(t *testing.T) {
	t.Run("clean turn then exit stays completed", func(t *testing.T) {
		st := session.New(session.Config{ID: "s1", Adapter: "claude"})
		srv := &Server{state: st}

		claudeEvent(t, srv, "UserPromptSubmit")
		if s := st.StatusSnapshot(); s == nil || !s.Active {
			t.Fatalf("UserPromptSubmit → %+v, want an open turn", s)
		}
		claudeEvent(t, srv, "Stop")
		if s := st.StatusSnapshot(); s == nil || s.Active || s.Error || s.Interrupted {
			t.Fatalf("Stop → %+v, want a clean completed closure", s)
		}
		if !st.UnreadSnapshot() {
			t.Fatal("Stop → completed must mark unread")
		}

		claudeEvent(t, srv, "SessionEnd")
		if s := st.StatusSnapshot(); s == nil || s.Interrupted || s.Error || s.Active {
			t.Fatalf("SessionEnd after Stop → %+v, want the completed closure preserved", s)
		}
		if !st.UnreadSnapshot() {
			t.Fatal("SessionEnd must not clear the completed turn's unread")
		}
	})

	t.Run("exit mid-turn is an interruption", func(t *testing.T) {
		st := session.New(session.Config{ID: "s1", Adapter: "claude"})
		srv := &Server{state: st}

		claudeEvent(t, srv, "UserPromptSubmit")
		claudeEvent(t, srv, "SessionEnd") // Ctrl+C / exit: Stop never fires
		s := st.StatusSnapshot()
		if s == nil || s.Active || !s.Interrupted || s.Error {
			t.Fatalf("SessionEnd mid-turn → %+v, want an interruption", s)
		}
		if st.UnreadSnapshot() {
			t.Fatal("an interrupted turn must not be unread")
		}
	})

	t.Run("exit with no turn reports no status", func(t *testing.T) {
		st := session.New(session.Config{ID: "s1", Adapter: "claude"})
		srv := &Server{state: st}

		claudeEvent(t, srv, "SessionEnd")
		if s := st.StatusSnapshot(); s != nil {
			t.Fatalf("SessionEnd with no turn → %+v, want no reported status", s)
		}
	})

	t.Run("repeated and late ends are idempotent", func(t *testing.T) {
		st := session.New(session.Config{ID: "s1", Adapter: "claude"})
		srv := &Server{state: st}

		claudeEvent(t, srv, "UserPromptSubmit")
		claudeEvent(t, srv, "SessionEnd") // genuine abort
		claudeEvent(t, srv, "SessionEnd") // duplicate delivery
		claudeEvent(t, srv, "Stop")       // late/out-of-order clean end
		s := st.StatusSnapshot()
		if s == nil || !s.Interrupted || s.Active {
			t.Fatalf("late ends rewrote the closure: %+v", s)
		}
		if st.UnreadSnapshot() {
			t.Fatal("a late completed end must not resurrect unread")
		}

		// A genuine new turn is still fully reported.
		claudeEvent(t, srv, "UserPromptSubmit")
		if s := st.StatusSnapshot(); s == nil || !s.Active || s.Interrupted {
			t.Fatalf("next turn → %+v, want a clean open turn", s)
		}
		claudeEvent(t, srv, "Stop")
		if s := st.StatusSnapshot(); s == nil || s.Active || s.Interrupted {
			t.Fatalf("next turn end → %+v, want completed", s)
		}
		if !st.UnreadSnapshot() {
			t.Fatal("the second turn's completion must mark unread")
		}
	})
}

// TestClaudeStopFailureIsTerminalError: an API-failed turn (rate limit, auth
// failure) ends as a terminal error, and the unconditional SessionEnd that
// follows on exit must not relabel it as an intentional interruption.
func TestClaudeStopFailureIsTerminalError(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "claude"})
	srv := &Server{state: st}

	claudeEvent(t, srv, "UserPromptSubmit")
	claudeEvent(t, srv, "StopFailure")
	s := st.StatusSnapshot()
	if s == nil || s.Active || !s.Error || s.Interrupted {
		t.Fatalf("StopFailure → %+v, want an inactive terminal error", s)
	}
	if !st.UnreadSnapshot() {
		t.Fatal("a failed turn produced an unconsumed result and must be unread")
	}

	claudeEvent(t, srv, "SessionEnd")
	if s := st.StatusSnapshot(); s == nil || !s.Error || s.Interrupted {
		t.Fatalf("SessionEnd after StopFailure → %+v, want the error preserved", s)
	}
}

// TestDelayedEndClosesTheWrongTurn documents the EXACT limit of the runner's
// polarity gate, so nobody mistakes it for turn identity: an end that is
// logically stale but arrives after a new turn started is indistinguishable
// from that new turn's end, and closes it.
//
// This is why senders must deliver hook events in order:
//   - pi serializes delivery on one promise chain, so request N+1 is never
//     issued before N settles (TestPiExtSerializesDelivery);
//   - Claude awaits its Stop hook, so a clean turn's end lands before what
//     follows; StopFailure's output is documented as ignored, so ordering
//     against a near-simultaneous SessionEnd is NOT guaranteed there. That
//     residual window is accepted, not claimed away.
//
// A turn token/generation would make the runner self-sufficient; it is
// deliberately out of scope here and this test is the marker for it.
func TestDelayedEndClosesTheWrongTurn(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}

	postHook(t, srv, []byte(`{"op":"turn","phase":"start"}`))                       // start₁
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","outcome":"completed"}`))   // end₁
	postHook(t, srv, []byte(`{"op":"turn","phase":"start"}`))                       // start₂
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","outcome":"interrupted"}`)) // delayed end₁

	s := st.StatusSnapshot()
	if s == nil || s.Active || !s.Interrupted {
		t.Fatalf("status = %+v: the gate is polarity-only, so the late end closes turn 2", s)
	}
	// What the gate DOES guarantee unconditionally: no end can reopen or
	// double-close a turn, so a further copy changes nothing. (Unread was
	// legitimately set by end₁'s completion and is user-facing attention
	// state, not turn state, so it stays set until the session is viewed.)
	unread := st.UnreadSnapshot()
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","outcome":"completed"}`))
	if s := st.StatusSnapshot(); s == nil || !s.Interrupted || s.Active {
		t.Fatalf("a further stale end mutated a closed turn: %+v", s)
	}
	if st.UnreadSnapshot() != unread {
		t.Fatalf("a stale end changed unread: %v → %v", unread, st.UnreadSnapshot())
	}
}

// TestPutStatusBypassesTheTurnGate: the generic child self-report channel is a
// raw whole-status write. A script may close a turn it never opened, and may
// clear the status entirely — the gate constrains hook turn ends only.
func TestPutStatusBypassesTheTurnGate(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "shell"})
	srv := &Server{state: st}

	put := func(body string) {
		rec := httptest.NewRecorder()
		srv.handlePutStatus(rec, httptest.NewRequest("PUT", "http://unix/status", bytes.NewReader([]byte(body))))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("PUT /status %s → %d", body, rec.Code)
		}
	}

	put(`{"active":false,"error":true}`) // no turn was ever open
	if s := st.StatusSnapshot(); s == nil || !s.Error {
		t.Fatalf("status = %+v, want the self-reported error", s)
	}
	put(`{"active":false,"interrupted":true}`)
	if s := st.StatusSnapshot(); s == nil || !s.Interrupted || s.Error {
		t.Fatalf("status = %+v, want a wholesale replacement", s)
	}
	put(`null`)
	if s := st.StatusSnapshot(); s != nil {
		t.Fatalf("status = %+v, want null to clear it", s)
	}
}

// TestTurnEndOutcomesOverTheHookWire pins the tool-neutral vocabulary end to
// end through the HTTP handler (not just applyTurnEnd): an open turn is
// required, and each outcome maps to exactly one durable state.
func TestTurnEndOutcomesOverTheHookWire(t *testing.T) {
	for _, tc := range []struct {
		outcome                    string
		wantErr, wantInt, wantRead bool
	}{
		{"completed", false, false, true},
		{"interrupted", false, true, false},
		{"error", true, false, true},
	} {
		st := session.New(session.Config{ID: "s1", Adapter: "pi"})
		srv := &Server{state: st}
		postHook(t, srv, []byte(`{"op":"turn","phase":"start"}`))
		postHook(t, srv, []byte(`{"op":"turn","phase":"end","outcome":"`+tc.outcome+`"}`))
		s := st.StatusSnapshot()
		if s == nil || s.Active || s.Error != tc.wantErr || s.Interrupted != tc.wantInt {
			t.Errorf("%s → %+v", tc.outcome, s)
		}
		if st.UnreadSnapshot() != tc.wantRead {
			t.Errorf("%s: unread=%v want %v", tc.outcome, st.UnreadSnapshot(), tc.wantRead)
		}
	}
}
