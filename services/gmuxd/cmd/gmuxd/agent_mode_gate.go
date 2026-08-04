package main

// agent_mode_gate.go — the ADR 0033 capability boundary for semantic agent
// actions, checked at the daemon BEFORE residency, readiness, or activity.
//
// "This session in this mode cannot do that" is a permanent-shaped fact:
// unlike a dead runner or a busy agent, no retry and no resume changes it.
// Checking it first also keeps transparent resume honest — without this
// gate, prompting a dead terminal claude session would spawn a real Claude
// TUI just to be refused by its runner one round-trip later.
//
// The runner keeps its own capability refusal (unsupported_adapter) as
// defense in depth; this gate exists so the refusal happens before any
// side effect and can name the mode boundary, which only the daemon —
// holding the session row's drive mode — can word correctly.

import (
	"fmt"
	"net/http"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// codeUnsupportedMode is the ADR 0033 §3 refusal: the harness has semantic
// support, but not in this session's drive mode. It is a sibling of
// unsupported_adapter (same 422, same permanent shape, checked in the same
// position); it is a distinct code because the message names a mode
// boundary, not a missing capability, and the CLI must not append the
// generic "no semantic support yet" hint to a boundary that is — for
// steer — permanent by design.
const codeUnsupportedMode = "unsupported_mode"

// codeNoTerminal is the inverse boundary: a PTY surface (raw input, the
// terminal WebSocket, scrollback/tail) addressed at an ACP-mode session,
// where no terminal exists at all (ADR 0033: "n/a", not "refused"). The
// message names the surfaces that do exist — the conversation view,
// gmux agent prompt, gmux agent logs.
const codeNoTerminal = "no_terminal"

// agentActionSteer distinguishes the permanently-refused steer wording from
// the convertible prompt/follow-up/cancel wording. Cancel reuses the
// non-steer message: like prompt, it gains a correct home in ACP mode.
func steerAction(mode string) bool { return mode == modeSteer }

// semanticModeGate is the production gate: adapter lookup by the row's
// recorded name, verdict from the pure refusal function below.
func semanticModeGate(row centralstore.Session, action string) (int, string, string) {
	return semanticModeRefusal(adapters.FindByAdapter(row.Adapter), row.Adapter, row.DriveMode, action)
}

// semanticModeRefusal reports why a semantic action against a session of
// this (adapter, drive mode) pair is refused, as (status, code, message),
// or a zero status when the action may proceed to delivery.
//
//   - An unknown adapter name defers, in either mode: the daemon must not
//     invent a capability verdict for an adapter it cannot see; the runner
//     refuses (or, pre-ACP-runner, fails loudly as outdated).
//   - ACP mode passes only for harnesses that HAVE an ACP drive mode
//     (claude, codex): capabilities attach to the (harness, mode) pair,
//     so a known pair like (pi, acp) or (shell, acp) — which nothing can
//     legitimately register today — is refused explicitly rather than
//     granted semantic delivery by mode alone.
//   - Terminal mode with an AgentActionEncoder (pi) passes.
//   - Terminal mode of an ACP-drivable harness (claude, codex) is refused
//     naming the mode boundary (ADR 0033 §3): steer permanently, the rest
//     until explicit mode conversion exists.
//   - Everything else (shell, editors) is refused with the adapter-level
//     message the runner would also give.
func semanticModeRefusal(a adapter.Adapter, name, driveMode, action string) (status int, code, msg string) {
	if a == nil {
		return 0, "", ""
	}
	if driveMode == centralstore.DriveModeACP {
		if adapter.SupportsACPDrive(a) {
			return 0, "", ""
		}
		return http.StatusUnprocessableEntity, codeUnsupportedMode, fmt.Sprintf(
			"%s has no ACP drive mode; this session's recorded mode does not exist for its harness, so semantic actions are refused rather than guessed at",
			name)
	}
	if _, ok := a.(adapter.AgentActionEncoder); ok {
		return 0, "", ""
	}
	if adapter.SupportsACPDrive(a) {
		if steerAction(action) {
			return http.StatusUnprocessableEntity, codeUnsupportedMode, fmt.Sprintf(
				"%s terminal sessions are interactive-only, and steering a foreign terminal is permanently unsupported: a mid-turn dialog can change what keystrokes mean. Use gmux attach to intervene by hand; ACP-mode %s sessions steer as a first-class request",
				name, name)
		}
		return http.StatusUnprocessableEntity, codeUnsupportedMode, fmt.Sprintf(
			"%s terminal sessions are interactive-only; semantic control requires the session in ACP mode. Use gmux send / gmux tail to drive this session, or gmux attach to take over",
			name)
	}
	// Mirrors the runner's unsupported_adapter refusal
	// (cli/gmux/internal/ptyserver/actions.go) so callers see one code for
	// one fact regardless of which layer answered first.
	return http.StatusUnprocessableEntity, codeUnsupportedAdapter, fmt.Sprintf(
		"adapter %s has no semantic agent actions; use raw input instead", name)
}
