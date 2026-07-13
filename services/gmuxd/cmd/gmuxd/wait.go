package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// handleWait implements POST /v1/sessions/{id}/wait?timeout=N. It blocks
// until the session is idle (Status.Working == false), the session
// dies, the optional timeout elapses, or the client disconnects.
//
// An optional output condition switches the wait from "agent idle" to
// "text appeared in the session's output": ?for_text=<substr> matches
// a fixed substring, ?for_regex=<pattern> a Go regexp. Output waits
// consume the on-disk scrollback tee the runner writes live into the
// session dir — the daemon-side source of truth for output bytes — so
// nothing can scroll past unseen between polls (loss is bounded by the
// scrollback cap, not the poll interval). See waitForOutput.
//
// The single signal we wait on is the per-session Status.Working flag
// — the turn state every session carries under the unified turn model:
// agent adapters flip it via their turn hooks, shell sessions via
// runner-tracked OSC 133 prompt marks, and sessions without either
// (one-shot commands) are Working from launch until their exit closes
// the lifetime turn (issue #373). Falling back to "no output bytes for
// N seconds" would race ad-hoc against tool-call progress prints; the
// explicit Working flag is what we already use in the UI's idle
// indicator and is the right thing to consume here.
//
// Every session is waitable. A markless interactive shell is Working
// for its whole life, so a wait on it blocks until the session exits —
// honest "never provably idle" behavior, bounded by --timeout.
//
// Reasons returned in the response body:
//
//   - "idle": the session's turn closed — Status.Working == false,
//     whether observed live or (via the Status persisted across death)
//     because the turn was already closed when the process exited: a
//     one-shot command completing, a shell exiting at its prompt, an
//     agent exiting after finishing its turn.
//   - "died": the session became unreachable with its turn still open
//     (crash mid-command / mid-turn), never demonstrated a turn state
//     at all, or was removed from the store (UI dismiss). Surfaces to
//     the CLI as exit code 2.
//
// HTTP status codes:
//
//   - 200 with {reason}: terminal state reached (caller maps to its
//     own exit code)
//   - 408 Request Timeout: --timeout deadline elapsed
//   - 404: session not found
//   - 400: bad timeout, bad regex, or both output conditions at once
//
// dirFor maps a session id to its per-session state dir (production:
// sessionmeta.Store.SessionDir); only output-condition waits read it.
func handleWait(w http.ResponseWriter, r *http.Request, sessions *store.Store, sessionID string, dirFor func(string) string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}

	if _, ok := sessions.Get(sessionID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}

	forText := r.URL.Query().Get("for_text")
	forRegex := r.URL.Query().Get("for_regex")
	if forText != "" && forRegex != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "for_text and for_regex are mutually exclusive")
		return
	}

	deadline, err := timeoutChan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if forText != "" || forRegex != "" {
		var match func(string) bool
		if forText != "" {
			match = func(line string) bool { return strings.Contains(line, forText) }
		} else {
			re, err := regexp.Compile(forRegex)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid regex: "+err.Error())
				return
			}
			match = re.MatchString
		}
		waitForOutput(w, r, sessions, sessionID, dirFor(sessionID), match, deadline)
		return
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
	// transition. Composing scripts want `gmux wait` to be a no-op
	// when the agent is already idle. (Note this is also why the bare
	// `send && wait` composition is racy: the "already idle" snapshot
	// may be the previous turn's. `gmux send --wait` routes through
	// handleInputWait instead, which requires a fresh Working pulse.)
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

// waitForOutput blocks until a rendered line of the session's output
// matches, the session dies, the optional deadline elapses, or the
// client disconnects.
//
// The bytes come from the on-disk scrollback tee the runner writes
// live into the session dir. Polling that file — instead of the store
// event bus or a client-side scrollback re-fetch — is what makes the
// condition sound: the file is append-only (with bounded rotation),
// so output can't slip past between polls; at worst a match is
// reported one tick late. Each poll replays the tee through a fresh
// terminal emulator and matches per rendered line, i.e. against the
// same ANSI-stripped text `gmux tail` prints. Matching raw PTY bytes
// instead would foot-trap agent TUIs, whose escape sequences routinely
// split a plain substring. Consequence: the pattern must fit on one
// rendered (wrapped) terminal line.
//
// The whole persisted scrollback is matched, not just bytes that
// arrive after the request: this mirrors the `until gmux tail | grep`
// loop the condition replaces, and avoids the race where the text
// appears between `gmux send` and `gmux wait`. Re-rendering is skipped
// while the scrollback files' sizes are unchanged, so an idle session
// costs two stat(2) calls per tick.
//
// Reasons: "matched" on success; "died" when the session exits or is
// removed before matching — with one final render first, so output
// that arrives in the same instant the session exits (the common case
// for `gmux -- <cmd>` one-shots) still counts as a match.
//
// "died" is gated on hasRunEvidence (the same predicate the idle wait
// uses): right after launch the session exists in the store with
// Alive == false until the runner's first upsert lands, and reporting
// "died" in that window would be a phantom death breaking the primary
// composition `gmux -d -- cmd && gmux wait $id --for-text …` (issue
// #216). See hasRunEvidence for the three signals that count as ran.
func waitForOutput(
	w http.ResponseWriter,
	r *http.Request,
	sessions *store.Store,
	sessionID string,
	dir string,
	match func(string) bool,
	deadline <-chan time.Time,
) {
	var lastSig scrollbackSig
	rendered := false

	// check re-renders (only when the scrollback files changed) and
	// reports a match. The change signature folds in modification time
	// as well as size, so a rotation that happens to land both files on
	// a previously-seen size pair still forces a re-render (the mtimes
	// advance) rather than silently hiding a match written across it.
	check := func() bool {
		sig := statScrollback(dir)
		if rendered && sig == lastSig {
			return false
		}
		lastSig, rendered = sig, true
		return outputMatches(dir, sessions, sessionID, match)
	}

	respond := func(reason string) {
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// diedUnlessFinalMatch does one last render before conceding death.
	// The output and the exit are separate writes (scrollback tee vs
	// store upsert) with no ordering between them, so matching final
	// bytes can land after this loop's top-of-iteration check() but
	// before we observe Alive == false. Without a final check() here,
	// `gmux -- <cmd>` one-shots that print their result in the same
	// breath as exiting would race to "died" (exit 2) instead of
	// "matched" (exit 0). check() re-renders because the final bytes
	// grew the scrollback file, so the just-appended output is seen.
	diedUnlessFinalMatch := func() {
		if check() {
			respond("matched")
			return
		}
		respond("died")
	}

	seenAlive := false
	for {
		if check() {
			respond("matched")
			return
		}
		cur, ok := sessions.Get(sessionID)
		if !ok {
			// Removed from the store (UI dismiss, prune): no more
			// output is ever coming, startup window or not.
			diedUnlessFinalMatch()
			return
		}
		seenAlive = seenAlive || cur.Alive
		// Same #216 startup-window gate the idle wait uses: don't call
		// a not-yet-started session dead. hasRunEvidence also covers the
		// force-dead / restart-restored shape (StartedAt set, no live
		// ExitCode) that a bare seenAlive||ExitCode check would hang on.
		if !cur.Alive && hasRunEvidence(cur, seenAlive) {
			diedUnlessFinalMatch()
			return
		}
		select {
		case <-r.Context().Done():
			// Client disconnected; nothing to write.
			return
		case <-deadline:
			writeError(w, http.StatusRequestTimeout, "timeout", "no matching output within timeout")
			return
		case <-ticker.C:
		}
	}
}

// statScrollback captures a change signature (size + mtime) for the
// session's scrollback files (previous + active). Missing files report
// size -2, and the caller's separate `rendered` flag guarantees the
// very first check always renders even when no scrollback exists yet.
type scrollbackSig struct {
	prevSize, prevModNs     int64
	activeSize, activeModNs int64
}

func statScrollback(dir string) scrollbackSig {
	statOf := func(name string) (int64, int64) {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return -2, 0 // missing: distinct from any real (size>=0) stat
		}
		return fi.Size(), fi.ModTime().UnixNano()
	}
	var s scrollbackSig
	s.prevSize, s.prevModNs = statOf(scrollback.PreviousName)
	s.activeSize, s.activeModNs = statOf(scrollback.ActiveName)
	return s
}

// outputMatches replays the session's persisted scrollback through a
// terminal emulator and reports whether any rendered line satisfies
// match. Terminal dimensions come from the session's last-known size
// (RenderTail falls back to 80x24 for sessions that never resized).
// Render errors report no-match: the wait keeps polling and, worst
// case, ends in timeout/died rather than a spurious success.
func outputMatches(dir string, sessions *store.Store, sessionID string, match func(string) bool) bool {
	rc, err := scrollback.OpenReader(dir)
	if err != nil || rc == nil {
		return false
	}
	defer rc.Close()

	var cols, rows int
	if sess, ok := sessions.Get(sessionID); ok {
		cols, rows = int(sess.TerminalCols), int(sess.TerminalRows)
	}
	if rows <= 0 {
		rows = 24
	}
	// The emulator retains at most RenderScrollbackSize scrolled-off
	// lines plus the visible screen; asking for that many lines back
	// means "everything the replay can know about".
	lines, err := scrollback.RenderTail(rc, cols, rows, scrollback.RenderScrollbackSize+rows)
	if err != nil {
		return false
	}
	for _, line := range lines {
		if match(line) {
			return true
		}
	}
	return false
}

// timeoutChan parses the optional ?timeout=N query parameter into a
// deadline channel. A nil channel (no timeout) blocks forever in a
// select, which is exactly the no-deadline behavior we want.
func timeoutChan(r *http.Request) (<-chan time.Time, error) {
	ts := r.URL.Query().Get("timeout")
	if ts == "" {
		return nil, nil
	}
	secs, err := strconv.Atoi(ts)
	if err != nil || secs <= 0 {
		return nil, errors.New("timeout must be a positive integer of seconds")
	}
	return time.After(time.Duration(secs) * time.Second), nil
}

// handleInputWait implements POST /v1/sessions/{id}/input?wait=idle:
// deliver input to the session and block until the turn it triggers
// completes (issue #218).
//
// The bare composition `gmux send <id> ... && gmux wait <id>` has an
// inherent race: `wait`'s initial snapshot can observe the *previous*
// turn's idle state before the send-induced Working=true has propagated
// from the runner's adapter into the store, returning "idle"
// immediately with stale output. The fix is ordering, done here where
// both halves live in one process: subscribe to the store BEFORE
// forwarding the input bytes, then require a fresh Working=true
// observation before any Working=false counts as "this turn is done".
// The Working pulse cannot be missed because the subscription predates
// the input delivery.
//
// Contract mirrors handleWait: 200 {reason: idle|died}, 408 on
// ?timeout=N elapsing. A 422 ("input_no_submit") rejects bodies that
// carry no carriage return: input that doesn't submit never starts a
// turn, so waiting on it would only ever time out — fail loudly at
// the edge instead. Every session is otherwise accepted; on a session
// that never closes its turn (markless interactive shell) the wait
// blocks until exit or --timeout, same as handleWait.
//
// send is a closure over the runner delivery (discovery.SendInput)
// so this handler — and its tests — stay independent of the socket
// transport.
func handleInputWait(w http.ResponseWriter, r *http.Request, sessions *store.Store, sess store.Session, body []byte, send func() error) {
	if mode := r.URL.Query().Get("wait"); mode != "idle" {
		// "idle" is the only wait mode today; the value exists so
		// future conditions (e.g. output text, #313) can slot in.
		// Rejecting unknown modes keeps a client typo from silently
		// degrading to fire-and-forget.
		writeError(w, http.StatusBadRequest, "bad_request",
			"unsupported wait mode "+strconv.Quote(mode)+`; expected "idle"`)
		return
	}
	if !inputSubmits(body) {
		writeError(w, http.StatusUnprocessableEntity, "input_no_submit",
			"input does not submit (no carriage return \\r or Enter key sequence; a bare newline \\n is treated as literal text, not a submit); "+
				"add a trailing Enter key or drop --wait")
		return
	}
	deadline, err := timeoutChan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Subscribe BEFORE delivering the input. This ordering is the
	// entire point of the endpoint: the Working=true pulse the input
	// triggers is broadcast to subscribers that already exist, so it
	// can't slip between "bytes delivered" and "wait armed".
	evCh, unsub := sessions.Subscribe()
	defer unsub()

	if err := send(); err != nil {
		writeError(w, http.StatusBadGateway, "runner_unreachable", err.Error())
		return
	}

	reason, timedOut := awaitTurn(r.Context(), sessions, evCh, sess.ID, deadline)
	switch {
	case timedOut:
		writeError(w, http.StatusRequestTimeout, "timeout", "session did not become idle within timeout")
	case reason != "":
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
		// reason == "" and !timedOut: client disconnected; nothing to write.
	}
}

// kittyEnterRe matches the Kitty keyboard protocol's CSI-u encoding of
// the Enter key (codepoint 13) with optional modifier and event-type
// fields: ESC [ 13 [;mod[:event]] u. `gmux send --follow-up` on a pi
// session sends Alt+Enter in this encoding (\x1b[13;3u) because pi
// parses it correctly under either negotiated keyboard protocol — see
// the pi adapter's SubmitSeq.
var kittyEnterRe = regexp.MustCompile(`\x1b\[13(?:;[0-9]+(?::[0-9]+)?)?u`)

// inputSubmits reports whether the input bytes contain a submit
// keystroke — something that can start a turn. A carriage return is
// what xterm-class terminals send for Enter (bare or Alt-modified); the
// CSI-u form is Enter under the Kitty keyboard protocol. Anything else
// is text the agent just buffers, so a --wait on it could only ever
// time out; handleInputWait rejects it up front.
func inputSubmits(body []byte) bool {
	return bytes.ContainsRune(body, '\r') || kittyEnterRe.Match(body)
}

// awaitTurn blocks until the session completes a turn: a Working=true
// observation followed by Working=false ("idle"), or the session dying
// or being removed at any point ("died"). Unlike terminalReason, a
// Working=false state observed before any Working=true does NOT
// terminate — that's exactly the stale previous-turn idle the caller
// subscribed early to skip past.
//
// Returns ("", false) when ctx is cancelled and ("", true) on deadline.
//
// Like handleWait, events are complemented by a periodic re-poll: the
// 64-slot subscriber buffers drop events under load, so both edges of
// the pulse could theoretically be missed. The poll recovers the
// Working=true edge whenever the turn outlasts one tick; a sub-tick
// turn whose both edge events were dropped is the one (vanishingly
// unlikely) case that rides out the deadline.
func awaitTurn(ctx context.Context, sessions *store.Store, evCh <-chan store.Event, sessionID string, deadline <-chan time.Time) (reason string, timedOut bool) {
	seenWorking := false
	check := func(s store.Session) (string, bool) {
		if !s.Alive {
			// No #216 startup-window gate here (unlike terminalReason /
			// hasRunEvidence): handleInputWait only runs after the input
			// handler's `!sess.Alive => 409` check, so we enter with a
			// live session and any Alive==false is a genuine exit. Like
			// terminalReason, the exit resolves by turn state: if a fresh
			// Working pulse was observed and the turn is closed at death
			// (the runner emits the close before the exit event), the
			// input's turn completed — e.g. input to a one-shot whose
			// processing ended when the process exited. Otherwise the
			// session died mid-turn.
			if seenWorking && s.Status != nil && !s.Status.Working {
				return "idle", true
			}
			return "died", true
		}
		if s.Status != nil && s.Status.Working {
			seenWorking = true
			return "", false
		}
		if seenWorking {
			// Status went non-working after a fresh Working pulse:
			// the turn our input triggered is over.
			if s.Status != nil && !s.Status.Working {
				return "idle", true
			}
		}
		return "", false
	}

	// Initial snapshot: catches a session already mid-turn (our bytes
	// queue behind the current turn; treat its end as the answer) or
	// already dead.
	if cur, ok := sessions.Get(sessionID); !ok {
		return "died", false
	} else if reason, done := check(cur); done {
		return reason, false
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-deadline:
			return "", true
		case ev := <-evCh:
			if ev.ID != sessionID {
				continue
			}
			if ev.Session == nil {
				// session-remove: dismissed mid-wait. See handleWait.
				return "died", false
			}
			if reason, done := check(*ev.Session); done {
				return reason, false
			}
		case <-ticker.C:
			cur, ok := sessions.Get(sessionID)
			if !ok {
				return "died", false
			}
			if reason, done := check(cur); done {
				return reason, false
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
// window is a phantom death (issue #216). A dead-session verdict
// therefore requires hasRunEvidence(s, seenAlive) — see that helper
// for the three signals that count as "this session actually ran."
//
// Death itself resolves by turn state (the Status the exit handling
// persists across death): a closed turn at exit means the session
// reached idle — a one-shot command completing, a shell exiting at
// its prompt, an agent exiting after its turn — and reports "idle"
// whether the wait watched it live or arrived afterwards. An open (or
// never-demonstrated) turn at death is a genuine "died".
func terminalReason(s store.Session, seenAlive bool) (string, bool) {
	if !s.Alive {
		if !hasRunEvidence(s, seenAlive) {
			return "", false
		}
		if s.Status != nil && !s.Status.Working {
			return "idle", true
		}
		return "died", true
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

