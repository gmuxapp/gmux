package main

import (
	"errors"
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
//   - 422: session adapter has no idle signal
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
	if !adapterEmitsIdleSignal(sess.Adapter) {
		writeError(w, http.StatusUnprocessableEntity, "no_idle_signal",
			"the "+sess.Adapter+" adapter does not emit an idle signal; --wait is only supported for agent sessions")
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

	// Whether we've observed this session with Alive == true at any
	// point during this wait. There is a startup window between
	// sessions.Register returning the id (so the CLI can resolve it)
	// and the runner flipping Alive to true; during that window
	// Alive == false means "not started yet", not "died". Gating
	// "died" on seenAlive keeps its contract as "was alive, isn't
	// anymore" — mirroring how a nil Status is treated as "no state
	// yet" rather than idle (see terminalReason).
	seenAlive := false

	// Already in a terminal state? Return without waiting for a
	// transition. Callers like `--send-wait` (composition) want
	// `--wait` to be a no-op when the agent is already idle.
	if cur, ok := sessions.Get(sessionID); ok {
		seenAlive = seenAlive || cur.Alive
		if reason, done := terminalReason(cur, seenAlive); done {
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
			seenAlive = seenAlive || ev.Session.Alive
			if reason, done := terminalReason(*ev.Session, seenAlive); done {
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
			seenAlive = seenAlive || cur.Alive
			if reason, done := terminalReason(cur, seenAlive); done {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
				return
			}
		}
	}
}

// Sentinel results for waitForSessionExit. Distinct errors let the
// restart handler report an honest status: a session that vanished
// mid-restart is a conflict with a concurrent dismiss/prune, not a
// kill that timed out.
var (
	errExitWaitTimeout = errors.New("session did not exit in time")
	errExitWaitRemoved = errors.New("session removed while waiting for exit")
)

// waitForSessionExit blocks until the session identified by sessionID
// has transitioned to its resumable exit state (Resumable, i.e.
// Alive == false with a non-empty resume Command — the store stamps
// this on every write), the overall timeout elapses, or the session
// disappears from the store. On success it returns the exited session
// snapshot; otherwise errExitWaitTimeout or errExitWaitRemoved.
//
// Callers subscribe BEFORE triggering the kill and pass the resulting
// channel here so the exit upsert can't be missed between subscribe and
// kill. store.broadcast drops events when a subscriber's 64-slot buffer
// is full, so under an event burst the exit upsert we're waiting on can
// be dropped. Mirroring handleWait, we re-poll the store every tick so a
// dropped event delays the result by at most one tick instead of
// timing out with a spurious kill_timeout — and, also like handleWait,
// we fail fast when the session is removed (UI dismiss, retention
// prune) rather than hanging out the full deadline for a session that
// can never become resumable.
func waitForSessionExit(sessions *store.Store, evCh <-chan store.Event, sessionID string, timeout, tick time.Duration) (store.Session, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return store.Session{}, errExitWaitTimeout
		case ev, ok := <-evCh:
			if !ok {
				return store.Session{}, errExitWaitTimeout
			}
			if ev.ID != sessionID {
				continue
			}
			if ev.Session == nil {
				// session-remove for our id: the store dropped the
				// session (a remove is never followed by a transient
				// re-upsert of the same id; shadow eviction only
				// removes other ids).
				return store.Session{}, errExitWaitRemoved
			}
			if ev.Session.Resumable {
				return *ev.Session, nil
			}
		case <-ticker.C:
			cur, ok := sessions.Get(sessionID)
			if !ok {
				return store.Session{}, errExitWaitRemoved
			}
			if cur.Resumable {
				return cur, nil
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
//
// The Alive flag needs the symmetric treatment: right after launch
// the session exists in the store with Alive == false until the
// runner's first upsert lands, and reporting "died" during that
// window is a phantom death (issue #216). "died" therefore requires
// hasRunEvidence(s, seenAlive) — see that helper for the three signals
// that count as "this session actually ran."
func terminalReason(s store.Session, seenAlive bool) (string, bool) {
	if !s.Alive {
		if hasRunEvidence(s, seenAlive) {
			return "died", true
		}
		return "", false
	}
	if s.Status != nil && !s.Status.Working {
		return "idle", true
	}
	return "", false
}

// hasRunEvidence reports whether a not-Alive session ever actually
// ran, which is what distinguishes a genuine death from the startup
// window where the session is registered but the runner's first
// upsert hasn't flipped Alive to true yet (issue #216). Evidence comes
// from any of:
//
//   - seenAlive: the caller observed Alive == true earlier in this
//     wait (tracked across its snapshot/event/poll observations), so a
//     later Alive == false is a true→false transition;
//   - ExitCode != nil: the runner watched the child process exit
//     (SetExited) — definitive even if this wait never saw it alive;
//   - StartedAt != "": the runner stamped SetRunning at some point.
//     Force-marked-dead sessions (unreachable runner on kill,
//     stale-socket sweep) and sessions restored from sessionmeta after
//     a daemon restart carry their historical StartedAt with no live
//     ExitCode, so a wait on them must fail fast rather than block for
//     a resurrection that can't come.
//
// A session with none of the three has never run: either it's in the
// startup window (common; the runner's next upsert resolves it) or its
// runner died before spawning the child (rare; bounded by --timeout).
// Shared by the idle wait (terminalReason) and the output-condition
// wait so the gate can't drift between them.
func hasRunEvidence(s store.Session, seenAlive bool) bool {
	return seenAlive || s.ExitCode != nil || s.StartedAt != ""
}

// adapterEmitsIdleSignal reports whether sessions of the given
// adapter ever transition Status.Working — i.e. whether --wait can
// observe a meaningful idle event for them. Currently the agent
// adapters (claude, codex, pi) all emit Working; the shell adapter
// doesn't. Kept as an explicit allowlist so adding a new agent
// adapter requires a deliberate update here, and so unknown adapters
// (peer sessions whose adapter we don't know about, future adapters)
// fail loudly instead of silently degrading to "always idle."
func adapterEmitsIdleSignal(adapter string) bool {
	switch adapter {
	case "claude", "codex", "pi":
		return true
	}
	return false
}
