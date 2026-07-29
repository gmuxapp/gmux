package discovery

// actions.go — client for the runner's semantic agent action routes
// (ADR 0027): POST /prompt and POST /cancel.
//
// These are deliberately NOT SendInput with parameters. Raw /input is
// unconditional byte delivery; the semantic routes name an intent, are gated
// on the agent's readiness and on an activity requirement the runner checks
// against authoritative state at the moment it commits to delivering, and they
// answer with a machine-readable refusal taxonomy. Nothing here is shared with
// SendInput beyond the AF_UNIX transport helper.
//
// Two properties matter to callers:
//
//   - success (204) means exactly "the whole payload reached the agent's
//     transport". It says nothing about acceptance (a turn starting) or
//     completion; observing those is the daemon's job, from status events.
//   - a refusal carries the runner's stable code, and whether bytes were
//     delivered is part of that code's contract, not something to guess from
//     the HTTP status. See RunnerActionError.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrRunnerSemanticActionsUnsupported reports that the runner answering an
// endpoint does not serve the semantic action routes at all: it predates them.
// It is a version fact, not a refusal — no bytes were delivered, and retrying
// against the same runner generation cannot help. The daemon turns it into a
// public "runner_outdated".
var ErrRunnerSemanticActionsUnsupported = errors.New("discovery: runner does not implement semantic agent actions")

// RunnerActionError is a structured refusal from a runner that understands the
// semantic routes. Code is the runner's stable code (ptyserver.Code*): it is
// passed through rather than reinterpreted, because only the runner knows
// whether bytes reached the agent, and a daemon that renamed or merged these
// codes would end up lying about delivery.
type RunnerActionError struct {
	Status  int
	Code    string
	Message string
}

func (e *RunnerActionError) Error() string {
	code := e.Code
	if code == "" {
		code = "runner_error"
	}
	if e.Message == "" {
		return fmt.Sprintf("runner refused semantic action: %s (HTTP %d)", code, e.Status)
	}
	return fmt.Sprintf("%s: %s", code, e.Message)
}

// promptBody is the runner's POST /prompt request. Every field is mandatory on
// the wire: the runner defaults nothing, deliberately.
type promptBody struct {
	Prompt   string `json:"prompt"`
	Delivery string `json:"delivery"`
	Require  string `json:"require"`
}

// SendPrompt delivers prompt text plus a submit-like action to the agent
// behind socketPath, and only if that endpoint is still owned by
// expectIncarnation.
//
// The expectation is mandatory, for the reason /reap exists: a socket pathname
// is reusable, so the process answering it now may not be the one the caller
// pinned. An unconditional prompt would then be executed by the replacement
// while the caller's wait, pinned to the predecessor, reported an
// indeterminate death. A mismatch comes back as ErrRunnerIncarnationMismatch
// and is a guaranteed non-delivery.
//
// delivery is "now" or "after_turn"; require is "inactive", "active" or "any".
// The values are passed through verbatim so the runner — not this client — is
// the only place that validates them: a value this daemon version does not
// know about must reach a runner that may, and an unknown value must fail
// loudly there rather than being defaulted here.
//
// ctx is the only bound on the call. The runner blocks for the adapter's
// readiness timeout (10 s for pi) before it refuses, and a PTY write can block
// on an agent that declines to read, so the ordinary 3 s runner request
// timeout would turn a legitimate slow admission into a transport error of
// indeterminate delivery. Callers must supply a deadline of their own.
func SendPrompt(ctx context.Context, socketPath, expectIncarnation, prompt, delivery, require string, _ ...string) error {
	body, err := json.Marshal(promptBody{Prompt: prompt, Delivery: delivery, Require: require})
	if err != nil {
		return err
	}
	return sendAction(ctx, socketPath, expectIncarnation, "/prompt", body)
}

// SendCancel delivers an interrupt to the agent behind socketPath. It carries
// no body: cancelling is not parameterized. The runner requires an active turn
// and returns as soon as the interrupt is delivered, without waiting for the
// agent to acknowledge by going inactive. Like SendPrompt, it is conditional on
// expectIncarnation still owning the endpoint.
func SendCancel(ctx context.Context, socketPath, expectIncarnation string) error {
	return sendAction(ctx, socketPath, expectIncarnation, "/cancel", nil)
}

func sendAction(ctx context.Context, socketPath, expectIncarnation, urlPath string, body []byte) error {
	if expectIncarnation == "" {
		// A programming error, not a runner fact: an unconditional semantic
		// action is not an operation this transport offers, and sending one
		// would risk executing a prompt in a process nobody decided about.
		return fmt.Errorf("discovery: %s to %s requires an expected incarnation", urlPath, socketPath)
	}
	var reader io.Reader
	header := http.Header{expectIncarnationHeader: []string{expectIncarnation}}
	if body != nil {
		reader = bytes.NewReader(body)
		header.Set("Content-Type", "application/json")
	}
	resp, err := runnerRequestUnbounded(ctx, socketPath, http.MethodPost, urlPath, reader, header)
	if err != nil {
		return fmt.Errorf("runner %s: %w", urlPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if semanticRouteMissing(resp.StatusCode) {
		return fmt.Errorf("%w: %s answered %s", ErrRunnerSemanticActionsUnsupported, urlPath, resp.Status)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	actErr := &RunnerActionError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Code != "" {
		actErr.Code = payload.Code
		actErr.Message = payload.Message
	}
	if actErr.Code == codeIncarnationMismatch {
		// Wrapped rather than passed through as an opaque refusal: this is the
		// one runner answer whose meaning is "your target is gone and nothing
		// was written", which the daemon must report as safe to retry instead
		// of lumping it in with the indeterminate codes.
		return fmt.Errorf("%w: %s", ErrRunnerIncarnationMismatch, actErr.Error())
	}
	return actErr
}

// codeIncarnationMismatch must match ptyserver.CodeIncarnationMismatch.
const codeIncarnationMismatch = "incarnation_mismatch"

// semanticRouteMissing reports whether a status means "this runner has no such
// route", as opposed to "it refused the request".
//
// 404 is the obvious answer, but it is not the one a real pre-0027 runner
// gives: those register a catch-all "/" route for the WebSocket terminal, so
// POST /prompt reaches the WebSocket handshake, which rejects a request
// without upgrade headers — 426 from nhooyr.io/websocket, with 405 and 501 the
// other plausible shapes of the same "wrong protocol at this path" verdict.
//
// The list is the same one conditional reaping needs (reapUnsupportedStatus)
// for the same reason, and is deliberately duplicated rather than shared: the
// two operations answer to different runner-version questions, and a future
// runner that grows one route without the other must be able to say so.
// Refusals (409, 422, 400) are never in here — a runner that answers those
// understood the request.
func semanticRouteMissing(code int) bool {
	switch code {
	case http.StatusNotFound, // no such route
		http.StatusMethodNotAllowed, // route exists, not for POST
		http.StatusUpgradeRequired,  // route exists only as a WebSocket upgrade
		http.StatusNotImplemented:   // route exists, operation does not
		return true
	}
	return false
}
