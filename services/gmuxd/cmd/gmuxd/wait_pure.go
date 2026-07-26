package main

import (
	"bytes"
	"errors"
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

func terminalReason(s compatSession, seenAlive bool) (string, bool) {
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
