package main

// steer_watch.go — "a user message injected into an active turn resolves every
// OTHER armed wait on that session early" (ADR 0027, "Steering interrupts
// waits").
//
// The rule exists because an injection changes the CONTRACT of the pending
// answer: the turn a waiter armed on was asked to do one thing, and after a
// steer (or a follow-up the agent merged into the same loop, or a human typing
// into the TUI) its answer speaks to something else. Serving that answer under
// exit 0 would be a plausible, silent lie, so the wait resolves early instead:
// exit 2, reason `steered`, and the turn keeps running so the caller can re-arm.
//
// Self-exclusion is the whole subtlety. The request that DID the injecting is
// waiting for exactly the merged close it caused, so it must not interrupt
// itself — but it may only claim that close under two conditions, both of which
// this file enforces from runner-asserted facts alone.
//
// A word on what the delivery id is: the adapter reports an injection's TEXT and
// nothing else, so the runner correlates that text with what gmux delivered and
// stamps the id only on an unambiguous match (see session.matchPendingLocked).
// It is an acknowledgement correlated by text, not an identity carried through
// the agent, and every ambiguity resolves to NO id — which lands here as an
// id-less injection: it interrupts everyone, and the callers who might have
// owned it report indeterminate. This layer therefore never has to weigh
// "probably mine".
//
//  1. ACKNOWLEDGED. The adapter reported an injection carrying this request's
//     delivery id, i.e. gmux's text demonstrably entered the loop. Delivery
//     alone proves bytes on a PTY, not consumption; a turn that settles before
//     the acknowledgement may have closed without ever seeing the text, so that
//     result is INDETERMINATE and never the pre-injection answer under exit 0.
//  2. LAST. A later injection — human or semantic — supersedes it. The newest
//     injection owns the answer, so the earlier injector is interrupted like any
//     other waiter, told that the turn was steered again after its message.
//
// Both are decided against the turn the waiter is BOUND to (turn_seq), never
// against "whatever is running now": an injection into a later turn is somebody
// else's business, and reacting to it would resolve a wait on evidence about a
// turn it never observed.

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// newDeliveryID mints an opaque identity for one delivery into a running turn.
//
// Random rather than a counter: the id is the only thing standing between "my
// steer landed" and "somebody else's did", it travels to a separate process, and
// a per-process counter would repeat across a daemon restart while a runner still
// held the older value in its correlation window. A crypto/rand read cannot fail
// in practice; if it ever did, an empty id degrades safely (the injector reports
// its result as indeterminate rather than claiming an answer).
func newDeliveryID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Public vocabulary for a wait that a steer resolved, and for an injector whose
// own text was never acknowledged.
const (
	// outcomeSteered is reported instead of a turn conclusion: the turn did not
	// end, it changed. It maps onto the interrupted exit code (2) at the CLI —
	// expected coordination, not a fault.
	outcomeSteered = "steered"
	// outcomeIndeterminate is the injector's honest non-answer: its message may
	// or may not have entered the loop that just closed, so no result may be
	// served for it in either direction.
	outcomeIndeterminate = "indeterminate"
	// causeSteeredAgain distinguishes the superseded injector ("your text went
	// in, then somebody steered again") from a plain foreign steer.
	causeSteeredAgain = "steered_again"
	// causeInjectionUnacknowledged marks the settle-before-ack race: the turn
	// closed without the adapter ever reporting this request's injection.
	causeInjectionUnacknowledged = "injection_unacknowledged"
)

// injectionWatch is one armed wait's view of the injections on the turn it is
// bound to.
//
// The zero value watches nothing (turnSeq 0 matches no turn), which is the right
// answer for a wait that never identified a turn: it has no answer to lose.
type injectionWatch struct {
	// turnSeq is the turn this wait is bound to, as the adapter identified it.
	turnSeq uint64
	// baseline is the turn's TOTAL injection count when this wait bound to it
	// (TurnCurrent.InjectionsSeen, a monotonic counter the runner never trims).
	// Injections that predate the wait did not change the contract the wait signed
	// up for — a `gmux wait` arriving into an already-steered turn asked about
	// THAT turn as it stands — so only newer ones interrupt.
	//
	// It is a counter and not len(Injections) on purpose: the frame's injection
	// LIST is bounded and drops its oldest entries, so on a saturated turn its
	// length stops growing and every later injection would read as "nothing new"
	// — inverting this rule into serving the merged answer under exit 0.
	baseline uint64
	// deliveryID is this wait's own injection identity, or "" for a wait that
	// injected nothing (a plain prompt, a follow-up delivered to an idle agent,
	// a bare `gmux wait`). An empty id deliberately matches no injection: a
	// human's message carries no id either, and treating "no id" as "mine"
	// would let every waiter claim a human's steer as its own.
	deliveryID string
}

// steerVerdict is what an injection watch concluded.
type steerVerdict struct {
	// Steered is true when the wait must resolve early.
	Steered bool
	// Text is the injected excerpt that resolved it — the "injected text" the
	// stderr report and the --json envelope carry.
	Text string
	// Cause is causeSteeredAgain when the interrupted waiter is the earlier
	// INJECTOR (its message went in and was then superseded), "" otherwise.
	Cause string
}

// check reports whether a frame's open turn interrupts this wait.
func (w injectionWatch) check(frame *sessioncoord.TurnFrame) steerVerdict {
	cur := frame.CurrentTurn(w.turnSeq)
	if cur == nil {
		return steerVerdict{}
	}
	return w.judge(cur.Injections, cur.InjectionsSeen())
}

// checkClose applies the same rule to a SETTLED turn's carried injections.
//
// It is not redundant with check: the injection event and the close can arrive
// in the same look (or the injection's own transient publish can be dropped
// under backlog), and a waiter that learns of the steer only at the close must
// still refuse to present the merged answer as the one it asked for.
func (w injectionWatch) checkClose(close *sessioncoord.TurnClose) steerVerdict {
	if close == nil || close.TurnSeq != w.turnSeq {
		return steerVerdict{}
	}
	return w.judge(close.Injections, close.InjectionsSeen())
}

// judge is the rule itself: `seen` decides whether anything NEW entered the loop
// (a monotonic count), and the bounded list supplies the text and the identity
// of the newest one.
func (w injectionWatch) judge(injections []sessioncoord.TurnInjection, seen uint64) steerVerdict {
	if seen <= w.baseline {
		return steerVerdict{} // nothing new entered the loop
	}
	if len(injections) == 0 {
		// The count says a message entered the loop but no text came with it.
		// The wait is still interrupted — the contract of the pending answer
		// changed either way — with a report that names no excerpt.
		return steerVerdict{Steered: true}
	}
	last := injections[len(injections)-1]
	if w.deliveryID != "" && last.DeliveryID == w.deliveryID {
		// Acknowledged AND last: this is the injector whose merged close is
		// still its own to claim.
		return steerVerdict{}
	}
	v := steerVerdict{Steered: true, Text: last.Text}
	if w.deliveryID != "" && w.acknowledged(injections) {
		v.Cause = causeSteeredAgain
	}
	return v
}

// acknowledged reports whether this wait's own injection is among the turn's
// RETAINED injections.
//
// On a turn steered more times than the frame retains, an injector's own record
// can have been evicted, and it then reads as unacknowledged rather than as
// superseded. Both are non-claims (indeterminate vs steered), so the loss costs
// a word in the report and never an answer.
func (w injectionWatch) acknowledged(injections []sessioncoord.TurnInjection) bool {
	if w.deliveryID == "" {
		return false
	}
	for _, inj := range injections {
		if inj.DeliveryID == w.deliveryID {
			return true
		}
	}
	return false
}

// mayClaimClose reports whether an INJECTING wait may be served the result of
// the close it observed, and why not when it may not.
//
// For a non-injecting wait (deliveryID == "") there is nothing to exclude and
// the ordinary turn-identity match decides, so it always may.
func (w injectionWatch) mayClaimClose(close *sessioncoord.TurnClose) (ok bool, cause string) {
	if w.deliveryID == "" {
		return true, ""
	}
	if close == nil || close.TurnSeq != w.turnSeq {
		// No settled record for our turn: the ordinary result-free path, which
		// says nothing about the injection either way.
		return true, ""
	}
	injections := close.Injections
	if len(injections) > 0 && injections[len(injections)-1].DeliveryID == w.deliveryID {
		return true, ""
	}
	if w.acknowledged(injections) {
		// Our text entered the loop and was then superseded: the answer belongs
		// to whoever injected last.
		return false, causeSteeredAgain
	}
	// The loop settled without ever acknowledging our text. pi may have closed
	// without consuming it, so neither "here is your answer" nor "your steer
	// failed" is assertable.
	return false, causeInjectionUnacknowledged
}

// closeVerdict is the single injection-aware decision at a turn's close: it
// reports the outcome that REPLACES the ordinary turn conclusion, or ("", "",
// "") when the conclusion stands and the result may be served.
//
// Two replacements exist, and the difference matters to the caller:
//
//   - steered (exit 2): a user message changed this turn's contract. For a
//     bystander that is any new injection; for an injector it is a LATER
//     injection superseding its own.
//   - indeterminate (exit 1): this injector's text was never acknowledged as
//     having entered the loop that just closed, so no claim is assertable in
//     either direction — above all not the pre-injection answer under exit 0.
func (w injectionWatch) closeVerdict(close *sessioncoord.TurnClose) (outcome, cause, text string) {
	if v := w.checkClose(close); v.Steered {
		return outcomeSteered, v.Cause, v.Text
	}
	ok, cause := w.mayClaimClose(close)
	switch {
	case ok:
		return "", "", ""
	case cause == causeSteeredAgain:
		// Superseded, but by an injection the baseline already covered (a wait
		// that bound after somebody else's steer and then steered itself). It is
		// the same fact as above and gets the same word.
		return outcomeSteered, cause, lastInjectionText(close)
	default:
		return outcomeIndeterminate, cause, ""
	}
}

func lastInjectionText(close *sessioncoord.TurnClose) string {
	if close == nil || len(close.Injections) == 0 {
		return ""
	}
	return close.Injections[len(close.Injections)-1].Text
}

// baselineInjections is the total injection count a frame's open turn already
// carries, which is what a wait binding to that turn must not react to.
func baselineInjections(frame *sessioncoord.TurnFrame, turnSeq uint64) uint64 {
	return frame.CurrentTurn(turnSeq).InjectionsSeen()
}
