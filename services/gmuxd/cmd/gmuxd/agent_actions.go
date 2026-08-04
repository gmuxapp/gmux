package main

// agent_actions.go — daemon orchestration for ADR 0027's semantic agent
// actions: POST /v1/sessions/{id}/prompt and POST /v1/sessions/{id}/cancel.
//
// The runner owns delivery (readiness gating, the activity requirement, the
// keystroke, the at-most-one-prompt-per-turn reservation). This file owns
// everything the runner deliberately cannot see:
//
//   - intent → mechanism. The public `mode` (prompt|follow_up|steer) becomes
//     the runner's (delivery, require) pair plus a resume policy. The runner
//     never learns which public mode produced its request, because the
//     plain/steer distinction is policy, not mechanism (ADR 0027 §5).
//   - transparent resume for plain/follow-up prompts on a dead retained
//     session, under the same session ID, via the lifecycle coordinator.
//   - admission: for a prompt delivered into an inactive agent, an
//     authoritative inactive→active transition. Delivery is bytes; admission
//     is the agent reacting. They are different facts and are reported as
//     different words.
//   - the fused wait: ONE outcome subscription established before delivery and
//     kept through active→inactive completion, so a turn that starts and ends
//     faster than the daemon can resubscribe cannot be missed.
//
// A completed synchronous turn also carries an `output` field, and it is the
// ADAPTER'S OWN assertion about that turn, relayed through the runner's turn
// frame (ADR 0027's 2026-07-28 amendment). This layer never reconstructs a
// result from the conversation: it serves the frame's close record, and only
// when that record's `turn_seq` matches the turn this request observed. A
// mismatch (two back-to-back turns between looks) degrades to a result-free
// close, never to the wrong turn's answer. `output` is present only for
// `outcome:"completed"`, and omitted (never empty) when the turn produced no
// prose — a tool-only turn, never transport loss.
//
// Deliberately absent: no peer forwarding, no durable operation records or
// phases. Peer-owned sessions are refused, not silently routed.
//
// Honesty rules that shape the response contract:
//
//   - never report a stronger fact than was observed. `admission:"delivered"`
//     means bytes reached the agent's transport; `admission:"accepted"` means a
//     fresh turn was authoritatively observed.
//   - never describe an indeterminate outcome as retryable. An admission
//     timeout means the bytes went in and no turn appeared: retrying may
//     duplicate the prompt, so its code and message say so.
//   - runner refusal codes are passed through, not reinterpreted; only the
//     runner knows whether it wrote anything.

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

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// Public modes accepted by POST /v1/sessions/{id}/prompt. There is no default:
// an unknown mode is rejected rather than degraded, because every degradation
// available here (treating a steer as a plain prompt, say) executes a
// different, unrequested intent.
const (
	modePrompt   = "prompt"
	modeFollowUp = "follow_up"
	modeSteer    = "steer"
)

// Runner-facing vocabulary. Duplicated as string constants rather than
// imported: cli/gmux and services/gmuxd are separate modules and the runner
// protocol is a wire contract, exactly like expectIncarnationHeader.
const (
	runnerDeliveryNow       = "now"
	runnerDeliveryAfterTurn = "after_turn"
	runnerRequireInactive   = "inactive"
	runnerRequireActive     = "active"
	runnerRequireAny        = "any"
)

// Stable public error codes for the semantic routes.
const (
	// codeRunnerOutdated — the runner serving this session predates the
	// semantic routes. Zero bytes delivered; the session must be restarted
	// (or driven with raw `gmux send`) to accept semantic actions.
	codeRunnerOutdated = "runner_outdated"
	// codeAdmissionTimeout — the prompt was delivered but no turn started
	// within the fixed admission window. INDETERMINATE: the agent may have
	// the text and simply not have reported a turn, so this is not a
	// safe-retry condition.
	codeAdmissionTimeout = "admission_timeout"
	// codeExecutionTimeout — the turn was admitted (or the action delivered)
	// and had not completed within the caller's timeout_seconds. The turn
	// keeps running; only the wait ended.
	codeExecutionTimeout = "execution_timeout"
	// codeLocalOnly — the reference names a peer-owned session. Semantic
	// actions are local-only in this slice, and forwarding them silently
	// would run somebody else's agent.
	codeLocalOnly = "local_only"
	// codeRunnerUnreachable — the runner's socket could not be reached, or
	// the transport failed before a status line. Zero bytes delivered.
	codeRunnerUnreachable = "runner_unreachable"
	// codeDeliveryTimeout — the runner call exceeded the daemon's fixed
	// delivery deadline. INDETERMINATE: the runner may have written the
	// prompt to the PTY already, so this is emphatically not
	// runner_unreachable and not a safe retry.
	codeDeliveryTimeout = "delivery_timeout"
	// codeIncarnationMismatch — the runner process this request was pinned to
	// had already been replaced at the endpoint by the time the action
	// reached it, and the replacement refused an action decided about its
	// predecessor. GUARANTEED NON-DELIVERY: zero bytes were written, which is
	// what makes it the one delivery failure in this taxonomy that is safe to
	// retry blindly.
	codeIncarnationMismatch = "incarnation_mismatch"
)

// Public outcome vocabulary. Runner death is an `error` with a cause, never a
// fourth outcome: from the caller's side a dead agent mid-turn and a failed
// turn are the same class of bad news, and inventing an outcome for it would
// force every consumer to grow a case for something the ADR calls an error.
const (
	outcomeCompleted   = "completed"
	outcomeError       = "error"
	outcomeInterrupted = "interrupted"
)

// causeRunnerDied marks an `error` outcome caused by the runner/session dying
// rather than by the agent reporting a failed turn.
const causeRunnerDied = "runner_died"

// admissionAccepted / admissionDelivered are the two admission facts.
const (
	admissionAccepted  = "accepted"
	admissionDelivered = "delivered"
)

// defaultAdmissionWindow is the fixed wait for an authoritative
// inactive→active transition after a prompt was delivered into an inactive
// agent (ADR 0027 §7). It is internal policy, not a public knob: a caller
// tuning it could only make acceptance detection wrong, and the number is a
// property of how fast an agent's hook reports a turn start, not of the
// caller's patience. Distinct from the caller's execution timeout, which
// starts where this one ends.
//
// 60s, widened from 10s on live evidence: a slow-loading model legitimately
// takes more than ten seconds between delivery and its first agent event, and an
// indeterminate answer for a healthy session is worse than a longer wait. The
// timeout remains indeterminate either way (§7).
const defaultAdmissionWindow = 60 * time.Second

// deliveryDeadline bounds ONE runner call (/prompt or /cancel).
//
// The runner legitimately blocks there: it waits for the adapter's readiness
// window (10 s for pi) and then performs a PTY write that an agent declining to
// read can stall indefinitely. Relying on the caller's context alone would make
// the daemon's own liveness a property of every client's timeout policy, so
// this is deliberate self-defense sized comfortably above the readiness
// window rather than a retry knob.
//
// It is a THIRD, distinct deadline: readiness/delivery is the runner's,
// admission is fixed policy here, execution is the caller's. Expiry after the
// runner call has begun is INDETERMINATE — bytes may already be on the PTY — so
// it is reported as delivery_timeout and never as runner_unreachable.
const deliveryDeadline = 30 * time.Second

// maxAgentTimeoutSeconds bounds timeout_seconds. Zero/absent means indefinite;
// anything above this is a caller mistake (a unit confusion, typically) and is
// rejected rather than silently clamped.
const maxAgentTimeoutSeconds = 86400

// maxPromptEnvelopeBytes caps the raw JSON body, and is deliberately looser
// than the 1 MiB decoded-prompt cap for the same reason the runner's is: JSON
// escaping expands a byte up to six-fold, so a legitimate 1 MiB prompt of
// control characters arrives as ~6 MiB of envelope.
const maxPromptEnvelopeBytes = 8 << 20

// agentPromptRequest is the validated public request.
type agentPromptRequest struct {
	Prompt string
	Mode   string
	// Wait defaults to true when the field is absent. The pointer preserves
	// the omission: `false` and "not given" must not collapse, because the
	// default is the non-empty behavior.
	Wait bool
	// ExecTimeout is zero for indefinite.
	ExecTimeout time.Duration
}

type agentPromptWire struct {
	Prompt         *string `json:"prompt"`
	Mode           *string `json:"mode"`
	Wait           *bool   `json:"wait"`
	TimeoutSeconds *int64  `json:"timeout_seconds"`
}

// agentDeps is the I/O boundary of the semantic action handlers: store reads,
// registry liveness, lifecycle resume, and the two runner calls. Tests
// substitute the boundary rather than a socket, which is what makes the race
// schedules deterministic.
type agentDeps struct {
	store            *centralstore.Store
	subscribe        func(ctx context.Context) ([]sessioncoord.Outcome, <-chan sessioncoord.Outcome, func(), error)
	live             func(id centralstore.SessionID) (sessioncoord.Runtime, bool)
	resume           func(ctx context.Context, id centralstore.SessionID) (sessioncoord.Runtime, error)
	sendPrompt       func(ctx context.Context, endpoint, incarnation, prompt, delivery, require string) error
	sendCancel       func(ctx context.Context, endpoint, incarnation string) error
	admissionWindow  time.Duration
	deliveryTimeout  time.Duration
	after            func(d time.Duration) <-chan time.Time
	resumeGuardError func(ctx context.Context, row centralstore.Session) (int, string, string)
	// frame reads the turn frame retained for a session's live generation. It is
	// the fallback carrier for every resolution path whose event did not bring
	// one (an initial fanout look, the ticker, a coalesced publish); nil means
	// "this deployment retains no frames", under which every close is served
	// result-free.
	frame func(id centralstore.SessionID) *sessioncoord.TurnFrame
}

// turnFrame reads the retained frame for a session, or nil.
func (d agentDeps) turnFrame(sid centralstore.SessionID) *sessioncoord.TurnFrame {
	if d.frame == nil {
		return nil
	}
	return d.frame(sid)
}

func productionAgentDeps(boot *Bootstrap, gmuxBin string) agentDeps {
	return agentDeps{
		store:     boot.Store,
		subscribe: boot.SubscribeOutcomes,
		live: func(id centralstore.SessionID) (sessioncoord.Runtime, bool) {
			return registryRuntime(boot.Registry, id)
		},
		resume: boot.Coordinator.Resume,
		sendPrompt: func(ctx context.Context, endpoint, incarnation, prompt, delivery, require string) error {
			return discovery.SendPrompt(ctx, endpoint, incarnation, prompt, delivery, require)
		},
		sendCancel: discovery.SendCancel,
		frame: func(id centralstore.SessionID) *sessioncoord.TurnFrame {
			return boot.Registry.Frame(id)
		},
		resumeGuardError: func(ctx context.Context, row centralstore.Session) (int, string, string) {
			return agentResumeGuard(ctx, boot, gmuxBin, row)
		},
	}
}

func (d agentDeps) window() time.Duration {
	if d.admissionWindow > 0 {
		return d.admissionWindow
	}
	return defaultAdmissionWindow
}

// delivery bounds one runner call. See deliveryDeadline.
func (d agentDeps) delivery() time.Duration {
	if d.deliveryTimeout > 0 {
		return d.deliveryTimeout
	}
	return deliveryDeadline
}

// generationLost reports whether the generation that received the bytes is no
// longer the installed live one. It is the arbiter for every ambiguous
// liveness signal on the outcome stream: a removal can race a replacement
// registration, and an outcome published with Alive=false can be overtaken by
// a new generation that is already installed by the time it is delivered.
// Consulting the registry answers "is MY generation still there", which is the
// only question a waiter cares about.
func (d agentDeps) generationLost(id centralstore.SessionID, deliveredGen uint64) bool {
	rt, live := d.live(id)
	if !live {
		return true
	}
	// An unknown delivered generation (a runtime that never carried one)
	// degrades to liveness alone rather than pretending to compare.
	return deliveredGen != 0 && rt.Generation != 0 && rt.Generation != deliveredGen
}

func (d agentDeps) timer(dur time.Duration) <-chan time.Time {
	if d.after != nil {
		return d.after(dur)
	}
	return time.After(dur)
}

// agentResumeGuard reports why a dead retained session cannot be resumed, as
// (status, code, message), or a zero status when resume may proceed. It mirrors
// POST /v1/sessions/{id}/resume's preconditions on purpose: a transparent
// resume must not be able to succeed where an explicit one would refuse.
func agentResumeGuard(ctx context.Context, boot *Bootstrap, gmuxBin string, row centralstore.Session) (int, string, string) {
	if row.ExitedAt == nil || len(row.Command) == 0 {
		return http.StatusBadRequest, "not_resumable", "session is not resumable"
	}
	if gmuxBin == "" {
		return http.StatusInternalServerError, "gmux_not_found", "gmux not found"
	}
	cwd, _, err := resolveResumeDirCentral(ctx, boot.Store, row)
	if err != nil {
		return http.StatusInternalServerError, "internal", err.Error()
	}
	if cwd == "" {
		return http.StatusUnprocessableEntity, "cwd_missing", "the session's working directory no longer exists and no fallback directory is available"
	}
	return 0, "", ""
}

// agentAction reports whether an action segment is one of the semantic agent
// routes. It exists so the peer-forwarding branch and the local dispatch can
// never disagree about which routes are local-only.
func agentAction(action string) bool {
	return action == "prompt" || action == "cancel"
}

// modePolicy maps a public mode onto the runner's mechanism plus this layer's
// resume policy.
//
//	mode       runner delivery  runner require  dead retained session
//	prompt     now              inactive        transparently resume
//	follow_up  after_turn       any             transparently resume
//	steer      now              active          fail; never resume
//
// Steer never resumes because it is meaningless: it steers a turn in progress,
// and a resumed session has no turn in progress. Cancel is the same and lives
// in the cancel handler.
// The ok result is separate from mayResume on purpose: steer legitimately has
// mayResume=false, and collapsing the two would make a valid mode look unknown.
func modePolicy(mode string) (delivery, require string, mayResume, ok bool) {
	switch mode {
	case modePrompt:
		return runnerDeliveryNow, runnerRequireInactive, true, true
	case modeFollowUp:
		return runnerDeliveryAfterTurn, runnerRequireAny, true, true
	case modeSteer:
		return runnerDeliveryNow, runnerRequireActive, false, true
	}
	return "", "", false, false
}

// handleAgentPromptCentral implements POST /v1/sessions/{id}/prompt.
func handleAgentPromptCentral(w http.ResponseWriter, r *http.Request, deps agentDeps, sessionID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	req, err := decodeAgentPrompt(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	delivery, require, mayResume, _ := modePolicy(req.Mode)
	sid := centralstore.SessionID(sessionID)
	row, found, err := deps.store.Session(r.Context(), sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	// The (adapter, drive mode) capability boundary is checked before
	// residency, readiness, and activity (ADR 0033 §3): it is the one
	// permanent-shaped refusal, and checking it here keeps transparent
	// resume from spawning an interactive session just to be refused.
	if status, code, msg := semanticModeGate(row, req.Mode); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	runtime, live := deps.live(sid)
	resumed := false
	if !live {
		if !mayResume {
			writeError(w, http.StatusConflict, "not_running", fmt.Sprintf(
				"session is not running; %s needs an active turn (resume the session and send a prompt instead)", req.Mode))
			return
		}
		if status, code, msg := deps.resumeGuardError(r.Context(), row); status != 0 {
			writeError(w, status, code, msg)
			return
		}
		runtime, live, err = resumeForPrompt(r.Context(), deps, sid)
		if err != nil {
			writeAgentResumeError(w, err)
			return
		}
		if !live {
			writeError(w, http.StatusServiceUnavailable, "busy",
				"session was resumed concurrently and has no live runner yet; retry")
			return
		}
		resumed = true
	}
	// Note on reporting: `resumed` rides the SUCCESS payload only. If a
	// transparent resume succeeds and delivery then fails, the error response
	// cannot mention it — the daemon's error envelope is
	// {"ok":false,"error":{code,message}} with no data slot, and growing one
	// just for this field would change every error response's shape for every
	// route. The effect is documented instead of designed around: a failed
	// prompt may leave the session resumed and live, which is observable
	// through ordinary session state, and is in any case the state an explicit
	// POST /resume would have produced.

	// Subscribe BEFORE delivery, and take the baseline FROM THE SUBSCRIPTION
	// SEED rather than from any earlier store read.
	//
	// Both halves are load-bearing:
	//
	//   - before delivery, because a turn can start AND finish before the
	//     runner's 204 reaches this process; a subscription established
	//     afterwards would see only the stale inactive tail.
	//   - from the seed, because SubscribeOutcomesSeed installs the subscriber
	//     and reads the rows under one publication fence AND records a
	//     per-session row-version watermark. Any event for a version already
	//     reflected in the seed is dropped at enqueue. So a status write that
	//     lands between an earlier store read and the subscription is
	//     unobservable in BOTH places: the read is too early to contain it and
	//     the event is suppressed as already-seen. Using the store read as the
	//     baseline therefore opens a permanent lost-edge gap. The seed is the
	//     only consistent starting point for edge detection.
	//
	// Store reads stay for existence and resume preconditions only.
	//
	// It is established after any resume, not before: a fresh generation's
	// launch-time status writes would otherwise be indistinguishable from the
	// prompt's own turn, and the seed then describes the generation this
	// prompt is actually delivered into.
	seed, outcomes, cancel, err := deps.subscribe(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer cancel()

	// Revalidate the runtime immediately before the call, and pin BOTH the
	// endpoint and the generation from that re-read.
	//
	// The first lookup happened before the resume decision and before the
	// subscription, and a socket pathname is reusable: a replacement runner
	// registering in that window means the earlier runtime names the old
	// generation while the bytes reach the new one. Judging observations against
	// the stale generation then filtered the real turn as "foreign" and returned
	// admission_timeout for a prompt that demonstrably ran.
	//
	// Re-pinning is not silently adopting somebody else's turn: it makes the
	// pinned generation the one this request is delivering into, and the
	// baseline is derived from the seed for that same generation immediately
	// below, so attribution stays internally consistent.
	//
	// The residual window between this read and the runner's own commit is
	// closed by the runner, not here: the incarnation pinned below travels with
	// the request, and a replacement that has taken over the pathname refuses it
	// with a guaranteed non-delivery (incarnation_mismatch) instead of executing
	// a prompt this process would then have attributed to the predecessor.
	runtime, live = deps.live(sid)
	if !live {
		// The generation went away before a single byte was sent. Nothing was
		// delivered, so this is safe to retry — and it is emphatically not
		// runner_died, which claims bytes are in flight. A resume is not
		// attempted again here: retrying lifecycle inside one request risks a
		// loop against a session that keeps dying.
		writeError(w, http.StatusConflict, "not_running",
			"the session's runner went away before the action could be delivered; nothing was delivered, so it is safe to retry")
		return
	}
	if runtime.Incarnation == "" {
		writeAgentUnidentifiedRunner(w)
		return
	}
	deliveredGen := runtime.Generation
	baselineActive := seedBaseline(seed, sid, deliveredGen)
	// A steer, or a follow-up merged into a running loop, joins a turn that is
	// ALREADY open: its identity is knowable now, from the frame, and that is the
	// turn whose close this request may claim. A plain prompt has no turn yet, so
	// it binds at the active edge instead (see runAgentWait) — binding it to
	// whatever is running now would let a stale seed bit hand it the PREVIOUS
	// turn's answer.

	var observedSeq uint64
	if baselineActive && req.Mode != modePrompt {
		observedSeq = deps.turnFrame(sid).CurrentTurnSeq()
	}
	var baselineOrdinal, anchorOrdinal uint64
	if current := deps.turnFrame(sid); current != nil && current.Current != nil && len(current.Current.Exchanges) > 0 {
		baselineOrdinal = current.Current.Exchanges[len(current.Current.Exchanges)-1].Ordinal
		if baselineActive {
			anchorOrdinal = baselineOrdinal
		}
	}
	spec := agentWaitSpec{
		baselineActive: baselineActive, baselineOrdinal: baselineOrdinal, anchorOrdinal: anchorOrdinal,
		// A plain prompt is admitted by the runner only against an inactive
		// agent, so a fresh turn is observable and is the acceptance fact. A
		// follow-up delivered to an idle agent submits immediately (ADR 0027
		// §6: "acts like ordinary send"), so it too can be accepted. A steer,
		// and a follow-up queued into a running turn, have no acknowledgement
		// separate from delivery.
		// A follow-up delivered to an IDLE agent starts an ordinary turn, so it
		// too has an observable acceptance. A follow-up delivered into a running
		// turn is merged into that loop by the agent (pi's queue): there is no
		// second turn to be admitted, and the merged close is the answer.
		requireAcceptance: req.Mode == modePrompt || (req.Mode == modeFollowUp && !baselineActive),
		wait:              req.Wait,
		admission:         deps.window(),
		execution:         req.ExecTimeout,
		generation:        deliveredGen,
		observedSeq:       observedSeq,
		frame:             func() *sessioncoord.TurnFrame { return deps.turnFrame(sid) },
		generationLost:    func() bool { return deps.generationLost(sid, deliveredGen) },
	}

	// The runner call gets a bounded context of its own (see deps.delivery):
	// ctx-only would let a wedged PTY write park this request for as long as
	// the client tolerates.
	dctx, releaseDelivery := context.WithTimeout(r.Context(), deps.delivery())
	err = deps.sendPrompt(dctx, runtime.Endpoint, runtime.Incarnation, req.Prompt, delivery, require)
	releaseDelivery()
	if err != nil {
		writeAgentDeliveryError(w, r, dctx, opPrompt, err)
		return
	}
	finishAgentAction(w, r, outcomes, sessionID, spec, resumed, deps.timer)
}

// seedBaseline derives this session's admission baseline from the atomic
// subscription seed.
//
// It answers exactly one question — "was the generation about to receive these
// bytes active at the moment the subscription was installed?" — and answers it
// conservatively (inactive) whenever the seed cannot speak for that
// generation:
//
//   - no seed row: nothing is known, and an inactive baseline only ever costs
//     one extra observed edge;
//   - the seed's liveness/generation disagrees with the generation being
//     delivered into: the seed describes somebody else's runner (typically the
//     dead predecessor of a session this request just resumed), whose
//     active-at-death bit is evidence about the PREVIOUS generation and must
//     never become a precondition on the new one;
//   - no runner-reported status yet: an unreported row proves nothing.
func seedBaseline(seed []sessioncoord.Outcome, id centralstore.SessionID, deliveredGen uint64) bool {
	for _, o := range seed {
		if o.ID != id || o.Type != sessioncoord.OutcomeUpserted || o.Session == nil {
			continue
		}
		if !o.Alive {
			return false
		}
		if deliveredGen != 0 && o.Generation != 0 && o.Generation != deliveredGen {
			return false
		}
		return o.Session.StatusReported && o.Session.Active
	}
	return false
}

// handleAgentCancelCentral implements POST /v1/sessions/{id}/cancel.
//
// Cancel is live-and-active only, never resumes, carries no body, and returns
// as soon as the interrupt is delivered — it deliberately does not wait for
// the agent to go inactive, because a cancel of a wedged agent would then
// hang, and `gmux wait` already expresses "tell me when it stopped".
//
// The active requirement is enforced by the runner, against authoritative
// state at the moment it commits, and is NOT pre-checked here: this process
// only has a coalesced view, and a local check could refuse a cancel the agent
// would have accepted (or accept one that turns into a stray Escape on an idle
// composer). Liveness is checked here because an endpoint is needed to call at
// all.
func handleAgentCancelCentral(w http.ResponseWriter, r *http.Request, deps agentDeps, sessionID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	if err := requireEmptyAgentBody(r); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sid := centralstore.SessionID(sessionID)
	// A store failure is not a missing session: reporting 404 for a broken read
	// would tell the caller their session is gone on the strength of a database
	// error.
	row, found, err := deps.store.Session(r.Context(), sid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	// Capability before residency/activity (ADR 0033 §3): "this mode cannot
	// cancel" is permanent-shaped; "no turn to cancel" is transient.
	if status, code, msg := semanticModeGate(row, opCancel); status != 0 {
		writeError(w, status, code, msg)
		return
	}
	runtime, live := deps.live(sid)
	if !live {
		writeError(w, http.StatusConflict, "not_running",
			"session is not running; there is no turn to cancel (cancel never resumes a session)")
		return
	}
	if runtime.Incarnation == "" {
		writeAgentUnidentifiedRunner(w)
		return
	}
	dctx, releaseDelivery := context.WithTimeout(r.Context(), deps.delivery())
	err = deps.sendCancel(dctx, runtime.Endpoint, runtime.Incarnation)
	releaseDelivery()
	if err != nil {
		writeAgentDeliveryError(w, r, dctx, opCancel, err)
		return
	}
	writeAgentJSON(w, http.StatusAccepted, map[string]any{"admission": admissionDelivered})
}

// finishAgentAction runs the admission and (optionally) the fused execution
// wait, then writes the response.
func finishAgentAction(w http.ResponseWriter, r *http.Request, outcomes <-chan sessioncoord.Outcome, sessionID string, spec agentWaitSpec, resumed bool, after func(time.Duration) <-chan time.Time) {
	res := runAgentWait(r.Context(), outcomes, sessionID, spec, after)
	switch res.Failure {
	case "":
	case codeAdmissionTimeout:
		writeError(w, http.StatusGatewayTimeout, codeAdmissionTimeout, fmt.Sprintf(
			"prompt was delivered but the agent did not start a turn within %s; delivery is indeterminate, so retrying may duplicate the prompt (inspect the session before resending)",
			spec.admission))
		return
	case codeExecutionTimeout:
		data := map[string]any{"admission": res.Admission, "outcome": "timeout", "cause": codeExecutionTimeout, "anchor_ordinal": res.AnchorOrdinal, "baseline_ordinal": spec.baselineOrdinal, "timeout_seconds": int(spec.execution / time.Second)}
		addCurrentExchangeData(data, spec.frame())
		writeAgentJSON(w, http.StatusGatewayTimeout, data)
		return
	case causeRunnerDied:
		if !spec.wait {
			writeError(w, http.StatusBadGateway, causeRunnerDied, "agent activity was lost after delivery")
			return
		}
		data := map[string]any{"admission": res.Admission, "outcome": outcomeError, "cause": causeRunnerDied, "anchor_ordinal": res.AnchorOrdinal, "baseline_ordinal": spec.baselineOrdinal}
		addCurrentExchangeData(data, spec.frame())
		writeAgentJSON(w, http.StatusBadGateway, data)
		return
	case "canceled":
		return // the caller went away; nothing to say to a closed connection
	default:
		writeError(w, http.StatusInternalServerError, "internal", res.Failure)
		return
	}
	data := map[string]any{"admission": res.Admission, "resumed": resumed, "anchor_ordinal": res.AnchorOrdinal, "baseline_ordinal": spec.baselineOrdinal}
	if !spec.wait {
		writeAgentJSON(w, http.StatusAccepted, data)
		return
	}
	data["outcome"] = res.Outcome
	if res.Cause != "" {
		data["cause"] = res.Cause
	}
	if res.Close != nil {
		if len(res.Close.Exchanges) == 0 && res.Close.Trigger != "" {
			data["trigger"] = res.Close.Trigger
		}
		if res.Close.PreviousExchanges != nil {
			data["previous_exchanges"] = *res.Close.PreviousExchanges
		}
		data["exchanges"] = res.Close.Exchanges
		data["omitted_exchanges"] = res.Close.OmittedExchanges
		data["omitted_bytes"] = res.Close.OmittedBytes
		if res.Close.Diagnostic != "" {
			data["diagnostic"] = res.Close.Diagnostic
		}
	}
	// The matched source close carries terminal prose for every outcome. The
	// shared renderer labels non-completed prose partial; an absent field means
	// the activity produced no prose, never transport loss.
	if res.Close != nil && res.Close.Output != "" {
		data["output"] = res.Close.Output
		if res.Close.Truncated {
			data["truncated"] = true
		}
	}
	writeAgentJSON(w, http.StatusOK, data)
}

// agentWaitSpec parameterizes one fused admission/execution wait.
type agentWaitSpec struct {
	// baselineActive is the activity of the generation the action is delivered
	// into, taken from the subscription seed (see seedBaseline). It is the
	// starting point for edge detection: without it, a steer (delivered into an
	// already-active turn) would resolve against the first inactive report it
	// saw, which may be a stale pre-delivery one.
	baselineActive bool
	// requireAcceptance demands a fresh inactive→active transition before the
	// action counts as admitted.
	requireAcceptance bool
	wait              bool
	admission         time.Duration
	execution         time.Duration // 0 = indefinite
	// Presentation ordinals are activity-monotonic, so frame eviction cannot
	// move the anchor or the delivery baseline onto another boundary.
	anchorOrdinal   uint64
	baselineOrdinal uint64
	// generation is the registry generation that received the bytes, or 0 when
	// the runtime carried none. Observations that demonstrably belong to a
	// different live generation are ignored.
	generation uint64
	// observedSeq is the identity of the turn this request may claim the result
	// of. It is pre-set only when the action joins a turn that is already open
	// (a steer, a merged follow-up); otherwise it is learned at the turn-start
	// edge. 0 means "no turn of ours is identified", which serves no result.
	observedSeq uint64
	// frame reads the retained turn frame. It is the fallback for a resolution
	// whose outcome carried no frame (a coalesced publish, a seeded look); nil
	// makes every close result-free.
	frame func() *sessioncoord.TurnFrame
	// generationLost consults the registry for the ambiguous liveness signals:
	// an outcome removal can race a replacement registration, and an outcome
	// stamped Alive=false can be stale by the time it is delivered. It is
	// mandatory: the single construction site always installs it.
	generationLost func() bool
}

// agentWaitResult is the observed result of one fused wait.
type agentWaitResult struct {
	Admission     string
	AnchorOrdinal uint64
	Outcome       string
	Cause         string
	// Close is the adapter's asserted record for the turn this wait observed,
	// or nil when no matching settled frame was available (a non-asserting
	// adapter, a raw PUT /status close, a version-skewed runner, or two
	// back-to-back turns between looks). nil means result-free, never wrong.
	Close *sessioncoord.TurnClose
	// Failure is "" on success, otherwise a stable public error code (or
	// "canceled" when the caller disconnected).
	Failure string
}

// runAgentWait is the state machine behind admission and fused completion. It
// is a pure function of an outcome stream and a timer factory so every race in
// ADR 0027 §7 can be scheduled deterministically in tests.
//
// Phases:
//
//	admission (optional) → execution
//
// Rules, each of which exists because the naive version is wrong:
//
//   - acceptance is an EDGE (inactive→active), not a value. A repeated
//     Active=true report is not a new turn.
//   - completion requires having observed active first. An inactive
//     observation before that is the pre-delivery state, not this turn ending.
//   - active+error stays active: an adapter reporting a retry/rate-limit
//     condition mid-turn has not finished.
//   - death at any point after delivery is an `error` outcome with cause
//     runner_died, including death while active+error and death before any
//     turn was observed — but only once the registry confirms the DELIVERED
//     generation is gone. A removal that races a replacement registration, or
//     an Alive=false outcome overtaken by a newly installed generation, is
//     stale and must not be reported as this session dying.
//   - observations belonging to another live generation are ignored outright:
//     after a takeover/restart, somebody else's turn is not this prompt's.
//   - completion is never reported for an acceptance-required mode that was
//     never admitted.
//   - the RESULT is served only on a turn-identity match. The wait records the
//     open turn's turn_seq (pre-set for a steer/merged follow-up, learned at the
//     active edge otherwise) and, at the close, accepts the frame's settled
//     record only when it names that same turn. There is no "newest answer"
//     path: a mismatch is served result-free.
//
// There is deliberately no queued-turn span any more. pi merges a follow-up
// delivered mid-turn into the running loop — one loop, one turn, one close whose
// answer IS the follow-up's — so the old two-close model (and its
// queued_turn_unobserved verdict) described a world the agent does not implement.
func runAgentWait(ctx context.Context, outcomes <-chan sessioncoord.Outcome, sessionID string, spec agentWaitSpec, after func(time.Duration) <-chan time.Time) agentWaitResult {
	admitted := !spec.requireAcceptance
	res := agentWaitResult{Admission: admissionDelivered, AnchorOrdinal: spec.anchorOrdinal}
	if admitted && !spec.wait {
		return res
	}
	active := spec.baselineActive
	sawActive := active
	observedSeq := spec.observedSeq
	// frameFor prefers the frame stamped on the resolving outcome (retained at
	// apply time for the generation that published it) and falls back to the
	// registry read for a resolution that carried none.
	frameFor := func(o sessioncoord.Outcome) *sessioncoord.TurnFrame {
		if o.Frame != nil {
			return o.Frame
		}
		if spec.frame == nil {
			return nil
		}
		return spec.frame()
	}

	var admissionDeadline <-chan time.Time
	if !admitted {
		admissionDeadline = after(spec.admission)
	}
	var execDeadline <-chan time.Time
	startExecution := func() {
		if spec.execution > 0 {
			execDeadline = after(spec.execution)
		}
	}
	if admitted {
		startExecution()
	}

	for {
		select {
		case <-ctx.Done():
			return agentWaitResult{Failure: "canceled"}
		case <-admissionDeadline:
			return agentWaitResult{Failure: codeAdmissionTimeout}
		case <-execDeadline:
			return agentWaitResult{Admission: res.Admission, AnchorOrdinal: res.AnchorOrdinal, Failure: codeExecutionTimeout}
		case o, ok := <-outcomes:
			if !ok {
				return agentWaitResult{Failure: "canceled"}
			}
			if string(o.ID) != sessionID {
				continue
			}
			foreign := o.Alive && spec.generation != 0 && o.Generation != 0 && o.Generation != spec.generation
			ambiguous := o.Type == sessioncoord.OutcomeRemoved || (o.Type == sessioncoord.OutcomeUpserted && !o.Alive)
			if foreign || ambiguous {
				// Both signals mean the same thing to this waiter — "something
				// happened to this session that was not my generation's turn" —
				// and both are decided by the same arbiter, the registry.
				//
				// A live outcome for ANOTHER generation is itself terminal
				// evidence when our generation is gone: a takeover/restart
				// publishes the replacement's rows and may publish no removal or
				// Alive=false outcome for the predecessor at all. Filtering it as
				// mere foreign attribution (the previous behavior) left a caller
				// without timeout_seconds waiting forever for a turn that could
				// never be reported. Only foreign ATTRIBUTION is filtered here —
				// never the loss of our own generation.
				//
				// Conversely, a removal published just before a replacement
				// registration (or a liveness stamp overtaken by one) is stale
				// while our generation is still installed, and must not be
				// reported as a death.
				if !spec.generationLost() {
					continue
				}
				res.Outcome, res.Cause = outcomeError, causeRunnerDied
				if !admitted {
					// The prompt's bytes went in and the runner died without
					// a turn ever being observed. That is an error outcome,
					// not an admission timeout: the cause is known.
					res.Admission = admissionDelivered
				}
				if !spec.wait {
					// A detached caller gets no outcome field, so "accepted"
					// alone would hide the death. Report it as a failure with
					// the same stable cause code instead.
					return agentWaitResult{Admission: res.Admission, Failure: causeRunnerDied}
				}
				return res
			}
			if o.Type != sessioncoord.OutcomeUpserted || o.Session == nil {
				continue
			}
			row := *o.Session
			if !row.StatusReported {
				// No authoritative status for this generation yet: it proves
				// neither a turn nor its absence.
				continue
			}
			if row.Active && !active {
				active, sawActive = true, true
				if frame := frameFor(o); res.AnchorOrdinal == 0 && frame != nil && frame.Current != nil && len(frame.Current.Exchanges) > 0 {
					res.AnchorOrdinal = frame.Current.Exchanges[len(frame.Current.Exchanges)-1].Ordinal
				}
				// The turn that just opened is the one this request may claim.
				// Learned here rather than pre-set, because a plain prompt has
				// no turn until this edge.
				if seq := frameFor(o).CurrentTurnSeq(); seq != 0 {
					observedSeq = seq
				}
				if !admitted {
					admitted = true
					admissionDeadline = nil
					res.Admission = admissionAccepted
					if !spec.wait {
						return res
					}
					startExecution()
				}
				continue
			}
			if !row.Active && active {
				active = false
			}
			if row.Active || !sawActive {
				continue
			}
			if spec.requireAcceptance && !admitted {
				// Completion is gated on admission for the modes whose
				// acceptance is observable: without an admitted turn there is
				// nothing of ours to have completed.
				continue
			}
			// Turn closed. Classification is shared with the generic wait
			// (classifyTurnClose) so the two paths cannot drift.
			res.Outcome = classifyTurnClose(row.Error, row.Interrupted)
			res.Close = frameFor(o).ClosedTurn(observedSeq)
			return res
		}
	}
}

// resumeForPrompt resumes a dead retained session, converging concurrent
// prompts for the same session onto one runner.
//
// The coordinator's per-session lifecycle claim already makes double-spawning
// impossible; what this adds is the caller-side interpretation of losing that
// race. ErrSessionAlive means somebody else's resume already installed a live
// generation — that is exactly the state this wanted, so the prompt proceeds
// against it. A claim held by another in-flight lifecycle operation is
// reported as busy rather than waited on: queueing here would mean a prompt
// silently blocked behind an unrelated restart.
//
// Duplicate PROMPTS are not prevented here and must not be: two concurrent
// prompts against one live agent are refused by the runner's delivery
// reservation (delivery_pending), which is the only place that knows whether
// bytes were written.
func resumeForPrompt(ctx context.Context, deps agentDeps, sid centralstore.SessionID) (sessioncoord.Runtime, bool, error) {
	runtime, err := deps.resume(ctx, sid)
	if err == nil {
		return runtime, true, nil
	}
	if errors.Is(err, sessioncoord.ErrSessionAlive) {
		runtime, live := deps.live(sid)
		return runtime, live, nil
	}
	return sessioncoord.Runtime{}, false, err
}

func writeAgentResumeError(w http.ResponseWriter, err error) {
	if errors.Is(err, sessioncoord.ErrNoRunnerSpawner) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeCentralLifecycleError(w, err)
}

// writeAgentDeliveryError maps a runner delivery failure onto the public
// contract without upgrading or downgrading what is known about delivery.
// op names the operation whose delivery failed, so the indeterminate wording
// describes what the caller actually asked for. A cancel timeout told to "check
// for a duplicated prompt" sends people hunting for something that was never
// sent.
func writeAgentDeliveryError(w http.ResponseWriter, r *http.Request, dctx context.Context, op string, err error) {
	if errors.Is(err, discovery.ErrRunnerSemanticActionsUnsupported) {
		writeError(w, http.StatusBadGateway, codeRunnerOutdated,
			"this session's runner predates semantic agent actions; restart the session (gmux restart) to use prompt/cancel, or drive it with raw `gmux send`")
		return
	}
	if errors.Is(err, discovery.ErrRunnerIncarnationMismatch) {
		// The runner that owned this endpoint was replaced before the action
		// reached it, and the replacement refused it. This is the safety
		// property working: nothing was delivered to either process, so unlike
		// every other delivery failure here, a retry is unconditionally safe.
		writeError(w, http.StatusConflict, codeIncarnationMismatch,
			"the session's runner was replaced before the action could be delivered, and the new runner "+
				"refused an action meant for its predecessor; nothing was delivered, so it is safe to retry")
		return
	}
	var actErr *discovery.RunnerActionError
	if errors.As(err, &actErr) {
		// The runner's code is the caller's answer: it encodes whether bytes
		// were delivered and whether a retry can help. Re-coding it here
		// would either lose that or claim something this process cannot know.
		writeError(w, agentRunnerStatus(actErr), actErr.Code, actErr.Message)
		return
	}
	if r != nil && r.Context().Err() != nil {
		// The caller hung up mid-delivery. Delivery is indeterminate, but
		// there is nobody left to tell.
		return
	}
	if dctx != nil && errors.Is(dctx.Err(), context.DeadlineExceeded) {
		// The daemon's own delivery deadline expired inside the runner call.
		// The runner may have written the bytes already, so this must not read
		// as "could not reach the runner" (which implies zero bytes) or as a
		// safe retry.
		writeError(w, http.StatusGatewayTimeout, codeDeliveryTimeout,
			deliveryTimeoutMessage(op))
		return
	}
	writeError(w, http.StatusBadGateway, codeRunnerUnreachable, err.Error())
}

// writeAgentUnidentifiedRunner refuses an action against a live runner that
// never reported an incarnation.
//
// Semantic delivery is conditional on runner identity, so an unidentifiable
// runner cannot be addressed safely at all: the only alternatives would be to
// send no expectation (which the runner refuses, and which would reopen the
// replacement window) or to guess. A runner old enough not to report an
// incarnation also predates /prompt and /cancel, so runner_outdated is the same
// fact the 404 path reports, reached one step earlier.
func writeAgentUnidentifiedRunner(w http.ResponseWriter) {
	writeError(w, http.StatusBadGateway, codeRunnerOutdated,
		"this session's runner did not identify itself, so it predates semantic agent actions; "+
			"restart the session (gmux restart) to use prompt/cancel, or drive it with raw `gmux send`")
}

// Operation names used in delivery-failure messages.
const (
	opPrompt = "prompt"
	opCancel = "cancel"
)

// deliveryTimeoutMessage words the indeterminacy for the operation that timed
// out. op is opPrompt or opCancel — the two call sites pass the constants — so
// prompt is the default rather than a third, unreachable "neutral" wording.
func deliveryTimeoutMessage(op string) string {
	if op == opCancel {
		return "the runner did not answer within the delivery deadline; the interrupt may already have been delivered, so the turn may still stop on its own (check the session's status before retrying the cancel)"
	}
	return "the runner did not answer within the delivery deadline; the prompt may already have been delivered, so retrying may duplicate it (inspect the session before resending)"
}

// agentRunnerStatus chooses the public HTTP status for a runner refusal.
// Statuses that already mean the same thing at this boundary are preserved;
// the runner's own 500 (an indeterminate PTY write) becomes 502, because the
// failure is upstream of this daemon, and its 400 becomes 500, because a
// runner rejecting gmuxd's envelope is a gmux bug, not a caller error.
func agentRunnerStatus(err *discovery.RunnerActionError) int {
	switch err.Status {
	case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusGatewayTimeout,
		http.StatusServiceUnavailable:
		return err.Status
	case http.StatusBadRequest:
		return http.StatusInternalServerError
	}
	return http.StatusBadGateway
}

func addCurrentExchangeData(data map[string]any, frame *sessioncoord.TurnFrame) {
	if frame == nil || frame.Current == nil {
		return
	}
	if frame.Current.PreviousExchanges != nil {
		data["previous_exchanges"] = *frame.Current.PreviousExchanges
	}
	data["exchanges"] = frame.Current.Exchanges
	data["omitted_exchanges"] = frame.Current.OmittedExchanges
	data["omitted_bytes"] = frame.Current.OmittedBytes
}

func writeAgentJSON(w http.ResponseWriter, status int, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}

// decodeAgentPrompt validates the public prompt body strictly: mandatory
// prompt and mode, no unknown fields, no trailing JSON, bounded envelope and
// decoded prompt, valid UTF-8, and no enum defaulting.
func decodeAgentPrompt(r *http.Request) (agentPromptRequest, error) {
	var out agentPromptRequest
	if err := checkAgentJSONMediaType(r); err != nil {
		return out, err
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPromptEnvelopeBytes+1))
	if err != nil {
		return out, fmt.Errorf("read body: %w", err)
	}
	if len(raw) > maxPromptEnvelopeBytes {
		return out, fmt.Errorf("request body exceeds %d bytes", maxPromptEnvelopeBytes)
	}
	if !utf8.Valid(raw) {
		// encoding/json silently substitutes U+FFFD for invalid UTF-8, which
		// would corrupt a caller's prompt under a success response. Refusing
		// their words beats rewriting them.
		return out, errors.New("request body is not valid UTF-8")
	}
	var wire agentPromptWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return out, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return out, errors.New("invalid JSON: trailing content after the request object")
	}
	if wire.Prompt == nil {
		return out, errors.New(`"prompt" is required`)
	}
	if *wire.Prompt == "" {
		return out, errors.New(`"prompt" must not be empty`)
	}
	if len(*wire.Prompt) > maxInputBytes {
		return out, fmt.Errorf("prompt exceeds %d bytes", maxInputBytes)
	}
	if wire.Mode == nil {
		return out, errors.New(`"mode" is required (prompt, follow_up or steer)`)
	}
	if _, _, _, ok := modePolicy(*wire.Mode); !ok {
		return out, fmt.Errorf("unknown mode %q; expected prompt, follow_up or steer", *wire.Mode)
	}
	out.Prompt, out.Mode = *wire.Prompt, *wire.Mode
	// Absent means true: the default is the synchronous, result-bearing form.
	out.Wait = wire.Wait == nil || *wire.Wait
	if wire.TimeoutSeconds != nil {
		secs := *wire.TimeoutSeconds
		if secs < 0 {
			return out, errors.New("timeout_seconds must not be negative (0 or absent means no timeout)")
		}
		if secs > maxAgentTimeoutSeconds {
			return out, fmt.Errorf("timeout_seconds must be at most %d", maxAgentTimeoutSeconds)
		}
		out.ExecTimeout = time.Duration(secs) * time.Second
	}
	return out, nil
}

// requireEmptyAgentBody accepts only an absent body or a literal empty object.
//
// The read limit is deliberately one byte past the cap and overflow is an
// error, not a truncation: truncating would let a body of padding followed by
// real content (`"    ...{\"mode\":\"steer\"}"`) read as blank and be accepted
// as "no options", which is the one thing this check exists to prevent.
func requireEmptyAgentBody(r *http.Request) error {
	const maxCancelBodyBytes = 1024
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCancelBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(raw) > maxCancelBodyBytes {
		return errors.New("cancel takes no request body")
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	return errors.New("cancel takes no request body")
}

// checkAgentJSONMediaType accepts a JSON content type, or none at all.
// Parameters (charset) are tolerated. It uses mime.ParseMediaType rather than a
// hand-rolled split so it agrees with the runner's identical check at the next
// hop (ptyserver.checkJSONMediaType): the two cannot share code across module
// boundaries, so sharing the parser is what keeps them from drifting apart on
// odd headers.
func checkAgentJSONMediaType(r *http.Request) error {
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if ct == "" {
		return nil
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return fmt.Errorf("invalid content type %q: %w", ct, err)
	}
	if mt != "application/json" {
		return fmt.Errorf("unsupported content type %q; expected application/json", mt)
	}
	return nil
}
