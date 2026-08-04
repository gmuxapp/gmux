package main

import (
	"bytes"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
)

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

func parseTimeoutDuration(raw, errorMessage string, unit time.Duration) (time.Duration, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || value > math.MaxInt64/int64(unit) {
		return 0, errors.New(errorMessage)
	}
	return time.Duration(value) * unit, nil
}

func timeoutChan(r *http.Request) (<-chan time.Time, error) {
	query := r.URL.Query()
	if ms := query.Get("timeout_ms"); ms != "" {
		duration, err := parseTimeoutDuration(ms, "timeout_ms must be a positive integer of milliseconds", time.Millisecond)
		if err != nil {
			return nil, err
		}
		return time.After(duration), nil
	}

	ts := query.Get("timeout")
	if ts == "" {
		return nil, nil
	}
	duration, err := parseTimeoutDuration(ts, "timeout must be a positive integer of seconds", time.Second)
	if err != nil {
		return nil, err
	}
	return time.After(duration), nil
}

// handleInputWait implements POST /v1/sessions/{id}/input?wait=idle:
// deliver input to the session and block until the turn it triggers
// completes (issue #218).
//
// The bare composition `gmux send <id> ... && gmux wait <id>` has an
// inherent race: `wait`'s initial snapshot can observe the *previous*
// turn's idle state before the send-induced Active=true has propagated
// from the runner's adapter into the store, returning "idle"
// immediately with stale output. The fix is ordering, done here where
// both halves live in one process: subscribe to the store BEFORE
// forwarding the input bytes, then require a fresh Active=true
// observation before any Active=false counts as "this turn is done".
// The Active pulse cannot be missed because the subscription predates
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

// awaitTurn blocks until the session completes a turn: an Active=true
// observation followed by Active=false ("idle"), or the session dying
// or being removed at any point ("died"). Unlike terminalReason, a
// Active=false state observed before any Active=true does NOT
// terminate — that's exactly the stale previous-turn idle the caller
// subscribed early to skip past.
//
// Returns ("", false) when ctx is cancelled and ("", true) on deadline.
//
// Like handleWait, events are complemented by a periodic re-poll. The
// poll is defence against a missed *activity* pulse only:
// sessioncoord's outcome bus keeps OutcomeUpserted/OutcomeRemoved on an
// unbounded, lossless queue (it is OutcomeActivity, at 256, that drops
// under load), and the semantic prompt path's edge detection depends on
// that losslessness. So do not read this as licence to distrust
// event-driven edge detection, or to re-add polling defences where
// Upserted outcomes are the evidence.

// waitConclusion is what a universal wait resolved to: the historical
// synchronization reason plus, for a wait that observed a turn close, the
// turn's conclusion in the SAME vocabulary the semantic prompt path reports
// (ADR 0027 §8/§11). Callers that only need synchronization can keep reading
// Reason; a caller that needs to know whether the turn actually succeeded
// reads Outcome.
type waitConclusion struct {
	// Reason is "idle", "died" or "matched" — unchanged from before.
	Reason string
	// Outcome is completed/error/interrupted, or "" for a wait that resolved
	// without observing a turn boundary (predicate waits).
	Outcome string
	// Cause names WHY an error outcome happened when the turn itself did not
	// report the failure (today: runner_died).
	Cause string
	// UnreadToken binds a successful consumer acknowledgement to the
	// exact result this verdict observed.
	UnreadToken string
}

// classifyTurnClose maps a closed turn's status flags onto the public outcome
// vocabulary. Shared with runAgentWait so the generic wait and the synchronous
// prompt can never disagree about what "completed" means: error wins over
// interruption (a turn that failed is not a clean stop), and neither flag means
// the turn completed.
func classifyTurnClose(hasError, interrupted bool) string {
	switch {
	case hasError:
		return outcomeError
	case interrupted:
		return outcomeInterrupted
	}
	return outcomeCompleted
}

// diedConclusion is the verdict for a session that went away: an error
// outcome caused by the death, never a fourth outcome and never "completed".
// A wait that resolves this way must not render a result — the newest stored
// message belongs to whatever ran before the death.
func diedConclusion() waitConclusion {
	return waitConclusion{Reason: "died", Outcome: outcomeError, Cause: causeRunnerDied}
}

func terminalReason(s compatSession, seenAlive bool) (waitConclusion, bool) {
	closed := func() waitConclusion {
		return waitConclusion{Reason: "idle", Outcome: classifyTurnClose(s.Status.Error, s.Status.Interrupted)}
	}
	if !s.Alive {
		if !hasRunEvidence(s, seenAlive) {
			return waitConclusion{}, false
		}
		// A session that died with its last turn closed still has a real
		// conclusion: the terminal flags of that turn. Dying mid-turn (or
		// before ever reporting a status, which proves neither a turn nor its
		// absence) is an error with a cause.
		if s.Status != nil && !s.Status.Active {
			return closed(), true
		}
		return diedConclusion(), true
	}
	if s.Status != nil && !s.Status.Active {
		return closed(), true
	}
	// Still active — including active+error, which is an attention-worthy
	// retry state and not a finished turn.
	return waitConclusion{}, false
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
//     stale-socket sweep) and sessions restored from prior durable state
//     after a daemon restart carry their historical StartedAt with no live
//     ExitCode, so a wait on them must fail fast rather than block for
//     a resurrection that can't come.
//
// A session with none of the three has never run: either it's in the
// startup window (common; the runner's next upsert resolves it) or its
// runner died before spawning the child (rare; bounded by --timeout).
// Shared by the idle wait (terminalReason) and the output-condition
// wait so the gate can't drift between them.
func hasRunEvidence(s compatSession, seenAlive bool) bool {
	return seenAlive || s.ExitCode != nil || s.StartedAt != ""
}
