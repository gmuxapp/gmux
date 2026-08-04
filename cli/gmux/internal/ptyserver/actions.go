package ptyserver

// Semantic agent actions (ADR 0027): POST /prompt and POST /cancel.
//
// These routes are permanently separate from raw POST /input, and the
// separation is the design, not an implementation accident:
//
//   - raw /input is unconditional. It writes the caller's bytes to the PTY
//     immediately, ignores readiness, takes no lock, checks no turn state and
//     reserves nothing. `gmux send` and a human at the keyboard are the same
//     thing, and both stay available when every semantic precondition refuses.
//   - /prompt and /cancel are *semantic*: the caller names an intent, the
//     adapter's AgentActionEncoder names the keystroke that expresses it, and
//     the runner refuses to write a single byte unless the agent has reported
//     itself ready and the caller's activity requirement holds at the moment
//     the runner commits to delivering.
//
// Nothing here is persisted or projected as session status. Readiness is
// runner-generation-local (see markReady), and so is the delivery reservation
// (session.State.AdmitAction).
//
// Old runners do not serve these paths, so a caller talking to one gets a
// plain 404. Translating that into an actionable "your runner is outdated"
// is the daemon's job, not this package's.
//
// # Ordering model
//
// Two mechanisms, always taken in this order, so no inversion is expressible:
//
//  1. s.deliverSlot — a capacity-1, context-aware semaphore. Holding it is
//     what makes semantic deliveries mutually exclusive and, crucially, what
//     makes the ORDER of the PTY writes match the order of the decisions. A
//     request waiting for it can be abandoned by its caller with zero bytes
//     delivered (that is why it is a channel and not a mutex).
//  2. session.State's status mutex, via State.AdmitAction — the runner's one
//     ordering mechanism for turn state. Every authoritative status writer
//     (agent hooks, OSC 133 prompt marks, PUT /status, the launch/exit
//     lifetime turn) goes through SetStatus/CloseTurnFrame, so the semantic
//     check-and-reserve is atomic against all of them.
//
// The linearization point of a semantic action is the AdmitAction that
// admits it — a reservation commit — NOT the PTY write that follows it. That
// is stated honestly rather than aspirationally: the write cannot be inside
// the status critical section, because a PTY write can block for as long as
// the agent declines to read, and blocking every status transition (including
// the hook events that would resolve the situation) behind it would be a
// self-inflicted deadlock. Consequences, both tested:
//
//   - a status transition cannot land between the check and the decision, and
//     two semantic actions cannot both admit on the same evidence. A request
//     that waited in the queue is decided on the state it is delivering into,
//     not on the state it saw when it arrived: the commit is the LAST
//     status-locked point before the write, with nothing in between.
//   - a transition that is genuinely concurrent with the write (an agent whose
//     hook reports its turn start before the runner's write call returns) is
//     recorded as reservation evidence and consulted once the write is known
//     to have completed — see session.State.ConfirmDelivery.
//
// # Scope of the guarantee
//
// The guarantee is: among semantic requests, absent concurrent raw
// intervention, at most one prompt is admitted per turn. It is NOT an
// at-most-once claim against a human or any other raw writer, and ADR 0027
// deliberately does not make one — a human at the keyboard, a WebSocket client
// and POST /input all write to the PTY without taking either mechanism,
// because raw input must stay unconditional.
//
// The sharpest case, written down so nobody has to rediscover it: a human can
// start (and finish) a turn while a semantic delivery is in flight. Without a
// turn token, an inactive→active edge racing the write is causally ambiguous —
// it may be our prompt's turn or theirs — and the runner assumes it is ours
// (see session.State.ConfirmDelivery). That resolves the ambiguity toward
// availability: assuming otherwise would wedge the common successful case,
// where the edge really is our prompt's turn, for the rest of the runner
// generation.
//
// # Blocked writes
//
// A PTY write can block for as long as the agent declines to read: the tty
// input buffer is small (~4 KiB on Linux), so the prompt size cap does NOT
// bound it, and an OS write cannot be safely cancelled once started. This is a
// contained availability limitation, stated plainly rather than argued away:
//
//   - the write holds only the delivery slot, so status transitions, raw
//     /input, the WebSocket and everything else keep flowing;
//   - further semantic requests queue behind it and their callers can cancel
//     with zero bytes delivered;
//   - no goroutine is leaked to write in the background (which would break both
//     the single-write guarantee and the ordering);
//   - a session in that state is recovered through raw input — unconditional,
//     and the same channel a human at the keyboard already has.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
)

// Stable machine-readable error codes returned in the JSON body of a failed
// semantic action. They exist so the daemon can map a runner refusal onto a
// public error without parsing prose.
const (
	// CodeInvalidRequest — malformed body, unknown field, unknown enum
	// value, empty prompt, invalid UTF-8, oversized prompt or envelope,
	// wrong media type. An unknown value NEVER degrades into a default: a
	// caller asking for something this runner does not understand must be
	// told, not guessed at.
	CodeInvalidRequest = "invalid_request"
	// CodeUnsupportedAdapter — this session's adapter implements no
	// semantic action support at all.
	CodeUnsupportedAdapter = "unsupported_adapter"
	// CodeUnsupportedAction — the adapter cannot express this particular
	// action.
	CodeUnsupportedAction = "unsupported_action"
	// CodeNotReady — the agent did not report readiness within the
	// adapter's ActionReadyTimeout. Guarantee: zero bytes were delivered,
	// so the caller may retry without risking a duplicate prompt.
	CodeNotReady = "not_ready"
	// CodePrecondition — the caller's `require` did not hold against the
	// authoritative status at commit time. Zero bytes delivered; retrying
	// is safe but will keep failing until the agent's activity changes.
	CodePrecondition = "precondition_failed"
	// CodeDeliveryPending — the agent is idle by status, but a prompt gmux
	// already delivered has not produced an observed turn yet, so this one
	// would be a duplicate. Distinct from CodePrecondition because the
	// caller's situation is different: nothing here will change until the
	// agent reacts (or a human intervenes via raw input), and a retry loop
	// on "agent is busy" would spin forever.
	CodeDeliveryPending = "delivery_pending"
	// CodeNotRunning — the child process exited before the action could be
	// delivered: the runner had not yet reported readiness and no bytes were
	// written. Guarantee: ZERO bytes delivered; the caller may retry safely.
	// Distinct from CodeTransportError, which is reserved for post-write /
	// indeterminate transport failures where some bytes may have reached the
	// agent.
	CodeNotRunning = "not_running"
	// CodeTransportError — the PTY write failed or was truncated. Delivery
	// is INDETERMINATE: some bytes may have reached the agent. Used ONLY
	// for failures that occur after the write has started; never for a
	// pre-write / pre-readiness child exit (CodeNotRunning owns that).
	CodeTransportError = "transport_error"
	// CodeIncarnationMismatch — the request named a different runner
	// process than the one that owns this endpoint, i.e. the caller's
	// intended target is already gone and its pathname was rebound.
	// Guarantee: ZERO bytes were delivered, so the caller may safely
	// re-decide against the current occupant. It is deliberately distinct
	// from every indeterminate code for exactly that reason.
	CodeIncarnationMismatch = "incarnation_mismatch"
)

// maxPromptBytes caps the DECODED prompt, in bytes. Same budget as raw /input:
// a semantic prompt is text that ends up on the same PTY, so the semantic path
// should accept neither more nor less than the raw one.
const maxPromptBytes = maxInputBytes

// maxPromptEnvelopeBytes caps the raw JSON request body. It is deliberately a
// different, looser limit than maxPromptBytes: JSON escaping expands a byte up
// to six-fold (`\u0000`), so a legitimate 1 MiB prompt of control characters
// arrives as ~6 MiB of envelope. Capping the envelope at the prompt limit would
// reject valid requests; not capping it at all would let a caller stream
// unbounded bytes into memory before validation. 8 MiB = 6× the prompt limit
// plus room for the field names and any future sibling fields.
const maxPromptEnvelopeBytes = 8 << 20

// Delivery modes accepted by POST /prompt. They map 1:1 onto the adapter's
// submit-like actions; the plain/steer distinction is NOT here, it is the
// `require` field, because both are the same keystroke (ADR 0027).
const (
	deliveryNow       = "now"
	deliveryAfterTurn = "after_turn"
)

// Activity requirements accepted by POST /prompt.
const (
	requireInactive = "inactive"
	requireActive   = "active"
	requireAny      = "any"
)

// promptRequest is the POST /prompt body. Every field is mandatory: there are
// no defaults, because a defaulted `require` would silently turn a caller's
// unrecognized intent into a different, executed one.
type promptRequest struct {
	Prompt   string
	Delivery string
	Require  string
}

// promptWire is the on-the-wire shape. Prompt is decoded as a raw token so the
// exact JSON string the caller sent can be validated before encoding/json gets
// a chance to rewrite it (see validateJSONStringToken).
type promptWire struct {
	Prompt   *json.RawMessage `json:"prompt"`
	Delivery string           `json:"delivery"`
	Require  string           `json:"require"`
}

var (
	errNotReady    = errors.New("agent did not report readiness")
	errChildExited = errors.New("child exited before the action could be delivered")
	errShortWrite  = errors.New("short write to the pty")
)

// markReady records that the agent reported itself ready to accept semantic
// input ({"op":"ready"} on /hook/event).
//
// Properties, all load-bearing:
//
//   - Idempotent. Repeat ready events are normal (an agent may report it on
//     every bind) and cost nothing.
//   - Unblocks every waiter at once — it closes a channel rather than
//     signalling one waiter, so N concurrent /prompt requests all proceed.
//   - Generation-local. It is never persisted, never emitted as session
//     status, and never reset by a conversation rebind: a rebind is the same
//     agent process with a different conversation, and its composer is still
//     accepting input. Only a new runner process (a restart/resume, which
//     builds a new Server) starts unready again.
func (s *Server) markReady() {
	s.readyOnce.Do(func() { close(s.readyCh) })
}

// ready reports whether the agent has reported readiness in this runner
// generation. Non-blocking; for tests and diagnostics.
func (s *Server) ready() bool {
	select {
	case <-s.readyCh:
		return true
	default:
		return false
	}
}

// awaitReady blocks until the agent is ready, the deadline expires, the child
// exits, or the caller goes away. On any error, no bytes have been written.
func (s *Server) awaitReady(ctx context.Context, timeout time.Duration) error {
	if s.ready() {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.readyCh:
		return nil
	case <-s.done:
		// Check readiness once more: a ready event and the child's exit can
		// land together, and a ready runner whose child just died should
		// fail on the write (transport) rather than claim it was never ready.
		if s.ready() {
			return nil
		}
		return errChildExited
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errNotReady
	}
}

// acquireSlot takes the delivery slot, or gives up when the caller does.
//
// This is why the slot is a channel rather than a sync.Mutex: a request queued
// behind a slow delivery must be abandonable with a guarantee that it wrote
// nothing, and Mutex.Lock cannot be cancelled.
func (s *Server) acquireSlot(ctx context.Context) error {
	select {
	case s.deliverSlot <- struct{}{}:
		return nil
	default:
	}
	select {
	case s.deliverSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) releaseSlot() { <-s.deliverSlot }

// actionBarrier runs the test barrier at the one point in the delivery sequence
// a schedule can be driven from: the delivery slot is held and nothing has been
// decided yet. It is deliberately the only such point — there is no seam between
// the commit and the write, because in production there is no window there
// either, and a test that parked in one would be pinning a delivery decided on
// state the runner had already been told was stale.
func (s *Server) actionBarrier() {
	if s.barrier != nil {
		s.barrier()
	}
}

// handlePrompt delivers prompt text plus a submit-like action to the agent.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	req, err := decodePromptRequest(r)
	if err != nil {
		writeActionError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	action := adapter.ActionSend
	if req.Delivery == deliveryAfterTurn {
		action = adapter.ActionSendAfterTurn
	}
	require, reserve := admissionPolicy(req.Delivery, req.Require)
	s.deliver(w, r, req.Prompt, action, require, reserve)
}

// handleCancel aborts the agent's current turn.
//
// It carries no body and no options: cancelling is not parameterized. It
// requires an active turn (there is nothing to abort otherwise, and pi's
// Escape on an idle composer means something else entirely), waits for
// readiness like every semantic action, and returns as soon as the interrupt
// is delivered — it deliberately does NOT wait for the agent to acknowledge by
// going inactive. That wait is a caller-facing concern (`gmux wait`), and
// blocking here would make a cancel of a wedged agent hang.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := requireEmptyBody(r); err != nil {
		writeActionError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	// An interrupt starts no turn, so it reserves nothing.
	s.deliver(w, r, "", adapter.ActionInterrupt, session.RequireActive, session.ReserveNever)
}

// admissionPolicy maps a validated request onto the state layer's requirement
// and reservation policy. Kept as one small total function because the
// reservation rules are the subtle part of ADR 0027's runner semantics:
//
//	require   delivery    requirement       reserves
//	inactive  now         RequireInactive    yes — this submit starts the next turn
//	active    now         RequireActive      no  — it steers a turn already running
//	any       now         RequireAny         only if the agent was idle at commit:
//	                                         idle → it starts the turn; busy → it
//	                                         joins the running one
//	*         after_turn  as above           yes — the queued text will run when the
//	                                         current turn ends, so the future turn
//	                                         is reserved even while the agent is busy
func admissionPolicy(delivery, require string) (session.TurnRequirement, session.ReservePolicy) {
	var req session.TurnRequirement
	switch require {
	case requireInactive:
		req = session.RequireInactive
	case requireActive:
		req = session.RequireActive
	default:
		req = session.RequireAny
	}
	pol := session.ReserveIfInactive
	if delivery == deliveryAfterTurn {
		pol = session.ReserveAlways
	}
	return req, pol
}

// deliver is the shared body of both semantic routes: capability → readiness →
// slot → commit → one write. See this file's header for the ordering model and
// for what is and is not guaranteed.
func (s *Server) deliver(w http.ResponseWriter, r *http.Request, prompt string, action adapter.AgentAction, require session.TurnRequirement, reserve session.ReservePolicy) {
	if err := s.requireIncarnation(w, r); err != nil {
		return
	}
	enc, ok := s.adapter.(adapter.AgentActionEncoder)
	if !ok {
		name := "this session"
		if s.adapter != nil {
			name = s.adapter.Name()
		}
		writeActionError(w, http.StatusUnprocessableEntity, CodeUnsupportedAdapter,
			fmt.Sprintf("adapter %s has no semantic agent actions; use raw input instead", name))
		return
	}
	input, ok := enc.EncodeAction(action)
	if !ok {
		writeActionError(w, http.StatusUnprocessableEntity, CodeUnsupportedAction,
			fmt.Sprintf("adapter %s cannot express %s", s.adapter.Name(), actionName(action)))
		return
	}

	ctx := r.Context()
	switch err := s.awaitReady(ctx, enc.ActionReadyTimeout()); {
	case err == nil:
	case errors.Is(err, errNotReady):
		writeActionError(w, http.StatusGatewayTimeout, CodeNotReady, fmt.Sprintf(
			"agent did not report readiness within %s; nothing was delivered", enc.ActionReadyTimeout()))
		return
	case errors.Is(err, errChildExited):
		// The child exited before reporting readiness: no bytes were written.
		// CodeNotRunning is the guaranteed-non-delivery code for this path;
		// CodeTransportError is reserved for post-write/indeterminate failures.
		writeActionError(w, http.StatusConflict, CodeNotRunning, errChildExited.Error())
		return
	default:
		return // caller hung up while waiting for readiness; no bytes delivered
	}

	// Queue for the delivery slot. Abandonable: a caller that hangs up here
	// has provably delivered nothing.
	if err := s.acquireSlot(ctx); err != nil {
		return
	}
	defer s.releaseSlot()
	s.actionBarrier()

	// Last cancellation check before anything irreversible. After the commit
	// below, cancellation is indeterminate — the bytes may already be gone —
	// so this is the point where "the caller went away" can still be honoured
	// cleanly.
	if err := ctx.Err(); err != nil {
		return
	}

	// Transport-start boundary: the requirement is (re)checked and the
	// reservation committed here, immediately before the write, under the
	// status lock. Nothing may be inserted between this call and the write.
	verdict, reserved := s.state.AdmitAction(require, reserve)
	if verdict != session.Admitted {
		status, code, msg := refusal(verdict)
		writeActionError(w, status, code, msg)
		return
	}
	if reserved {
		// Safety net for an unwind that never reaches confirm/clear below (a
		// panic in the transport, say): an orphaned in-flight reservation
		// would refuse every later prompt for the rest of the runner
		// generation, with no request left to resolve it. It is a no-op after
		// either normal outcome, because both end the in-flight phase.
		defer s.state.AbandonInFlightReservation()
	}

	// One ordered write, prompt text first. Two writes would let the agent
	// observe (and act on) a submit before the whole prompt arrived, which for
	// a multiline prompt means submitting half of it; and the prompt is
	// written verbatim, never parsed as key tokens, so a prompt that happens
	// to contain "\r" or an escape sequence is the caller's text, not a
	// second action.
	payload := []byte(prompt + input)
	n, err := s.deliverBytes(payload)
	if err == nil && n != len(payload) {
		// A truncated payload is not a delivery: the agent holds a fragment of
		// a prompt, possibly without its submit keystroke. Report it as a
		// transport failure rather than 204.
		err = fmt.Errorf("%w: wrote %d of %d bytes", errShortWrite, n, len(payload))
	}
	if reserved {
		if err != nil {
			// Undo our own reservation: the payload did not go out whole, and
			// holding a reservation for a delivery that did not happen would
			// refuse every later prompt for the rest of the generation. Safe
			// because we still hold the delivery slot, so no other semantic
			// action can have taken a reservation since.
			s.state.ClearReservation()
		} else {
			// The whole payload went out. End the in-flight phase: this
			// releases the reservation if the agent's turn start already
			// raced our write, and otherwise leaves it held until such an
			// edge arrives.
			s.state.ConfirmDelivery()
		}
	}
	if err != nil {
		writeActionError(w, http.StatusInternalServerError, CodeTransportError,
			"write pty: "+err.Error()+" (delivery is indeterminate: some bytes may have reached the agent)")
		return
	}
	// Successfully delivered prompt/follow-up/steer input consumes the previous
	// result. Interrupt is control, not consumption: canceling current work does
	// not prove the caller read an earlier result.
	if action != adapter.ActionInterrupt {
		s.state.SetUnread(false)
	}
	// 204 means the bytes reached the PTY transport — nothing more. Whether
	// the agent accepted them (inactive→active) or completed the turn is
	// observed elsewhere, from status events.
	w.WriteHeader(http.StatusNoContent)
}

// requireIncarnation binds a semantic action to the runner process the caller
// meant, and it is mandatory on both routes.
//
// The reason is the same one that gave /reap its own route: a socket pathname
// is reusable, so the occupant at delivery time may not be the process the
// caller decided about. The daemon pins a runtime (endpoint + generation),
// then calls the endpoint; if the pinned generation exits and a replacement
// binds the same pathname in that window, an unconditional /prompt would be
// *executed* by the replacement while the daemon's wait, still pinned to the
// predecessor, reported runner_died. A real side effect reported as an
// indeterminate failure, which a blind retry then duplicates.
//
// There is no mixed-version hazard in making the expectation mandatory:
// /prompt and /cancel ship together with this check, so any runner that serves
// these routes at all also enforces it, and a runner that predates them
// answers 404/426 ("runner_outdated") rather than silently ignoring a header.
// A missing header is therefore a caller bug, not an older client.
//
// Refusal happens before capability, readiness, the delivery slot and the
// admission commit, so its guarantee is the strongest one this file offers:
// zero bytes were written, and the caller may safely re-pin and retry.
func (s *Server) requireIncarnation(w http.ResponseWriter, r *http.Request) error {
	want := r.Header.Get(ExpectIncarnationHeader)
	if want == "" {
		err := errors.New("semantic actions require " + ExpectIncarnationHeader)
		writeActionError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return err
	}
	if want != s.incarnation {
		err := errors.New("incarnation mismatch: this pathname is owned by a different runner; nothing was delivered")
		writeActionError(w, http.StatusConflict, CodeIncarnationMismatch, err.Error())
		return err
	}
	return nil
}

// refusal maps an admission verdict onto the wire.
func refusal(v session.Admission) (status int, code, msg string) {
	switch v {
	case session.RefusedActive:
		return http.StatusConflict, CodePrecondition, "agent is active"
	case session.RefusedInactive:
		return http.StatusConflict, CodePrecondition, "agent is not active"
	case session.RefusedPending:
		return http.StatusConflict, CodeDeliveryPending,
			"a previously delivered prompt has not produced an observed turn yet; " +
				"retrying would duplicate it"
	}
	// Unreachable: deliver() only calls this for a refusal.
	return http.StatusConflict, CodePrecondition, "action refused"
}

func actionName(a adapter.AgentAction) string {
	switch a {
	case adapter.ActionSend:
		return "send"
	case adapter.ActionSendAfterTurn:
		return "send-after-turn"
	case adapter.ActionInterrupt:
		return "interrupt"
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// decodePromptRequest parses and strictly validates a POST /prompt body.
// Anything it does not understand is an error: this is a command surface, and
// a silently-defaulted command is a command the caller did not issue.
func decodePromptRequest(r *http.Request) (promptRequest, error) {
	var req promptRequest
	if err := checkJSONMediaType(r); err != nil {
		return req, err
	}
	// Read one byte past the envelope cap so an over-cap body is refused
	// rather than silently truncated into shorter valid-looking JSON.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPromptEnvelopeBytes+1))
	if err != nil {
		return req, errors.New("read error: " + err.Error())
	}
	if len(body) > maxPromptEnvelopeBytes {
		return req, fmt.Errorf("request body exceeds %d bytes", maxPromptEnvelopeBytes)
	}
	// Validate the ORIGINAL bytes as UTF-8 before decoding. encoding/json
	// silently substitutes U+FFFD for invalid UTF-8 in strings, which would
	// deliver mangled text under a successful 204 — for a prompt, corrupting
	// the caller's words is worse than refusing them.
	if !utf8.Valid(body) {
		return req, errors.New("request body is not valid UTF-8")
	}
	var wire promptWire
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return req, errors.New("invalid JSON: " + err.Error())
	}
	// Exactly one JSON value, so `{...}{...}` (or trailing junk) is rejected
	// rather than half-executed.
	if dec.More() {
		return req, errors.New("invalid JSON: trailing content after the request object")
	}
	req.Delivery, req.Require = wire.Delivery, wire.Require
	if wire.Prompt == nil {
		return req, errors.New("prompt is empty")
	}
	// Validate the caller's own prompt token, then decode it. Order matters:
	// encoding/json silently turns an unpaired surrogate escape into U+FFFD,
	// and a prompt whose words have been rewritten must be refused rather than
	// delivered under a 204.
	if err := validateJSONStringToken(*wire.Prompt); err != nil {
		return req, err
	}
	if err := json.Unmarshal(*wire.Prompt, &req.Prompt); err != nil {
		return req, errors.New("invalid prompt: " + err.Error())
	}
	if req.Prompt == "" {
		return req, errors.New("prompt is empty")
	}
	// The limit that matters is on what we deliver, not on the envelope that
	// carried it.
	if len(req.Prompt) > maxPromptBytes {
		return req, fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	}
	switch req.Delivery {
	case deliveryNow, deliveryAfterTurn:
	default:
		return req, fmt.Errorf("delivery must be %q or %q, got %q", deliveryNow, deliveryAfterTurn, req.Delivery)
	}
	switch req.Require {
	case requireInactive, requireActive, requireAny:
	default:
		return req, fmt.Errorf("require must be %q, %q or %q, got %q", requireInactive, requireActive, requireAny, req.Require)
	}
	return req, nil
}

// validateJSONStringToken checks the caller's prompt token for the one thing
// encoding/json accepts but silently rewrites: `\uXXXX` escapes that do not
// form a valid scalar.
//
// It looks at the token for `prompt` only, and it is structural rather than a
// guess about the decoded result: an unpaired high surrogate (`\ud800` not
// followed by a low surrogate escape) and a stray low surrogate (`\udc00`) are
// rejected; a correct pair (`\ud83d\ude00`) is accepted; and `\ufffd`, or a
// literal U+FFFD in the body, is just a character the caller wants — accepted
// verbatim, even alongside an escape that needed pairing.
//
// Everything else about the token (unknown escapes, raw control characters,
// unterminated strings, a non-string value) is left to encoding/json, which
// rejects those loudly instead of rewriting them.
//
// Why structural rather than the shorter "count U+FFFD in the decoded text and
// compare against the count the caller asked for": the cheaper version is a
// heuristic about a result, this is a decision about the input. The refusal is
// part of a delivery taxonomy where "invalid_request" must mean "deterministically
// refused, zero bytes delivered" for exactly the inputs the caller can reason
// about — lone surrogates and truncated escapes — and a counting rule's answer
// depends on how encoding/json happened to render the damage. The extra lines buy
// that determinism.
func validateJSONStringToken(tok []byte) error {
	if len(tok) < 2 || tok[0] != '"' {
		return errors.New("prompt must be a JSON string")
	}
	body := tok[1 : len(tok)-1]
	for i := 0; i < len(body); {
		if body[i] != '\\' {
			i++
			continue
		}
		if i+1 >= len(body) {
			break // truncated escape: encoding/json's error to report
		}
		if body[i+1] != 'u' {
			i += 2 // some other escape; not our concern
			continue
		}
		r, ok := parseHex4(body[i+2:])
		if !ok {
			return errors.New("invalid \\u escape in prompt")
		}
		i += 6
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return errors.New("prompt contains an unpaired low surrogate escape")
		case r >= 0xD800 && r <= 0xDBFF:
			// Must be followed by a low surrogate escape.
			low, ok := parseUEscape(body[i:])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return errors.New("prompt contains an unpaired high surrogate escape")
			}
			i += 6
		}
	}
	return nil
}

// parseUEscape reads a `\uXXXX` escape at the start of b.
func parseUEscape(b []byte) (rune, bool) {
	if len(b) < 6 || b[0] != '\\' || b[1] != 'u' {
		return 0, false
	}
	return parseHex4(b[2:])
}

// parseHex4 reads exactly four hex digits.
func parseHex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		var v rune
		switch {
		case c >= '0' && c <= '9':
			v = rune(c - '0')
		case c >= 'a' && c <= 'f':
			v = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = rune(c-'A') + 10
		default:
			return 0, false
		}
		r = r<<4 | v
	}
	return r, true
}

// requireEmptyBody enforces that POST /cancel carries no payload. An empty
// body and a bare `{}` are both accepted (HTTP clients differ on whether they
// can POST nothing); anything else is a caller who thinks cancel takes
// options, and must be corrected rather than have them ignored.
func requireEmptyBody(r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		return errors.New("read error: " + err.Error())
	}
	switch strings.TrimSpace(string(body)) {
	case "", "{}":
		return nil
	}
	return errors.New("cancel takes no request body")
}

// checkJSONMediaType accepts a JSON content type, or none at all. Parameters
// (charset) are tolerated; a different type is refused, because a caller
// announcing form data or plain text has almost certainly built the wrong body.
func checkJSONMediaType(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return nil
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return errors.New("invalid Content-Type: " + err.Error())
	}
	if mt != "application/json" {
		return fmt.Errorf("Content-Type must be application/json, got %q", mt)
	}
	return nil
}

// writeActionError emits the machine-readable error body the semantic routes
// return. Raw /input keeps its plain-text http.Error responses: it has no
// error taxonomy to communicate, while every semantic failure here means
// something specific to the caller (retryable or not, delivered or not).
func writeActionError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}
