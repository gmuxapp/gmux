package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// handleWait implements POST /v1/sessions/{id}/wait?timeout=N. It blocks
// until the session is idle (Status.Working == false), the session
// dies, the optional timeout elapses, or the client disconnects.
//
// The single signal we wait on is the per-session Status.Working flag
// the adapters emit. That's a precise, debounced signal: each adapter
// flips it false once its agent has finished its turn (claude /
// codex / pi all emit a Working transition). Falling back to "no
// output bytes for N seconds" would race ad-hoc against tool-call
// progress prints; the explicit Working flag is what we already use
// in the UI's idle indicator and is the right thing to consume here.
//
// Sessions whose adapter doesn't emit Working (notably the shell
// adapter) are rejected with 422 rather than silently returning
// "idle" immediately, which would foot-trap the obvious composition
// `gmux make build && gmux --wait <id>` into doing nothing.
//
// Reasons returned in the response body:
//
//   - "idle": session reached Status.Working == false
//   - "died": session is no longer reachable before becoming idle.
//     This covers Alive flipping to false (the session crashed or its
//     adapter exited) and the session being removed from the store
//     (UI dismiss, or any other code path calling sessions.Remove).
//     Both surface to the CLI as exit code 2; --wait callers don't
//     need to distinguish them.
//
// HTTP status codes:
//
//   - 200 with {reason}: terminal state reached (caller maps to its
//     own exit code)
//   - 408 Request Timeout: --timeout deadline elapsed
//   - 422: session kind has no idle signal
//   - 404: session not found
func handleWait(w http.ResponseWriter, r *http.Request, sessions *store.Store, sessionID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}

	sess, ok := sessions.Get(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	if !kindEmitsIdleSignal(sess.Kind) {
		writeError(w, http.StatusUnprocessableEntity, "no_idle_signal",
			"session kind "+sess.Kind+" does not emit an idle signal; --wait is only supported for agent sessions")
		return
	}

	var deadline <-chan time.Time
	if ts := r.URL.Query().Get("timeout"); ts != "" {
		secs, err := strconv.Atoi(ts)
		if err != nil || secs <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "timeout must be a positive integer of seconds")
			return
		}
		deadline = time.After(time.Duration(secs) * time.Second)
	}

	// Subscribe BEFORE re-reading current state. If we read first then
	// subscribed, an event between the read and the subscribe would be
	// lost and we'd block forever waiting for a transition that already
	// happened.
	evCh, unsub := sessions.Subscribe()
	defer unsub()

	// Already in a terminal state? Return without waiting for a
	// transition. Callers like `--send-wait` (composition) want
	// `--wait` to be a no-op when the agent is already idle.
	if cur, ok := sessions.Get(sessionID); ok {
		if reason, done := terminalReason(cur); done {
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
			return
		}
	}

	// Subscriber channels are buffered (64 slots) and broadcast() drops
	// events when the buffer is full. Under heavy load (e.g. parallel
	// orchestration with many agents emitting status events) the
	// critical Working→false transition for our session could
	// theoretically be among the dropped events. Re-snapshot every
	// poll interval so a missed event delays the response by at most
	// one tick instead of hanging forever.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected; nothing to write.
			return
		case <-deadline:
			writeError(w, http.StatusRequestTimeout, "timeout", "session did not become idle within timeout")
			return
		case ev := <-evCh:
			if ev.ID != sessionID {
				continue
			}
			if ev.Session == nil {
				// session-remove: the session was dismissed (UI close)
				// or otherwise dropped from the store. From --wait's
				// perspective this is indistinguishable from a crash:
				// the agent is no longer reachable and won't ever
				// reach idle. Without this case --wait would hang
				// forever (the ticker fallback also returns no-op for
				// missing sessions), which is exactly the failure mode
				// no-default-timeout was meant to avoid.
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "died"}})
				return
			}
			if reason, done := terminalReason(*ev.Session); done {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
				return
			}
		case <-ticker.C:
			cur, ok := sessions.Get(sessionID)
			if !ok {
				// Session removed from the store between events;
				// see the session-remove case above.
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "died"}})
				return
			}
			if reason, done := terminalReason(cur); done {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
				return
			}
		}
	}
}

// terminalReason inspects a session and reports whether --wait should
// return now and with what reason. Centralised so the initial-snapshot
// check, the per-event check, and the periodic re-poll stay in sync.
//
// A nil Status means the adapter hasn't emitted any status yet — the
// session has been registered in the store but the runner's first
// /events frame hasn't reached us. Treating that as idle would
// foot-trap `gmux pi <prompt> --no-attach && gmux --wait $id`: --wait
// would race the runner's first Working:true event and return
// immediately without the agent having processed anything. Wait
// instead for the adapter to assert a real state.
func terminalReason(s store.Session) (string, bool) {
	if !s.Alive {
		return "died", true
	}
	if s.Status != nil && !s.Status.Working {
		return "idle", true
	}
	return "", false
}

// kindEmitsIdleSignal reports whether sessions of the given adapter
// kind ever transition Status.Working — i.e. whether --wait can
// observe a meaningful idle event for them. Currently the agent
// adapters (claude, codex, pi) all emit Working; the shell adapter
// doesn't. Kept as an explicit allowlist so adding a new agent
// adapter requires a deliberate update here, and so unknown kinds
// (peer sessions whose adapter we don't know about, future adapters)
// fail loudly instead of silently degrading to "always idle."
func kindEmitsIdleSignal(kind string) bool {
	switch kind {
	case "claude", "codex", "pi":
		return true
	}
	return false
}
