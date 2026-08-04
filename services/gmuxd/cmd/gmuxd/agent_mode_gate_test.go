package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionmeta"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// The pure verdict table: which (adapter, drive mode, action) pairs pass to
// delivery and which are refused with which code. Adapters are looked up by
// name through the production path so the table breaks if an adapter's
// capability set changes underneath it.
func TestSemanticModeRefusalTable(t *testing.T) {
	tests := []struct {
		name       string
		adapter    string
		driveMode  string
		action     string
		wantStatus int
		wantCode   string
		wantIn     string
	}{
		{"pi terminal prompt passes", "pi", "terminal", modePrompt, 0, "", ""},
		{"pi terminal steer passes", "pi", "terminal", modeSteer, 0, "", ""},
		{"claude terminal prompt refused at the mode boundary", "claude", "terminal", modePrompt,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "interactive-only; semantic control requires the session in ACP mode"},
		{"claude terminal follow-up refused at the mode boundary", "claude", "terminal", modeFollowUp,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "gmux send / gmux tail"},
		{"claude terminal steer refused permanently", "claude", "terminal", modeSteer,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "permanently unsupported"},
		{"claude terminal cancel refused at the mode boundary", "claude", "terminal", opCancel,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "interactive-only"},
		{"codex terminal steer refused permanently", "codex", "terminal", modeSteer,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "permanently unsupported"},
		{"codex terminal prompt refused at the mode boundary", "codex", "terminal", modePrompt,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "ACP mode"},
		{"shell terminal prompt refused as unsupported adapter", "shell", "terminal", modePrompt,
			http.StatusUnprocessableEntity, codeUnsupportedAdapter, "no semantic agent actions"},
		// ACP mode passes only for harnesses that HAVE an ACP drive mode:
		// their runner owns the semantic verbs.
		{"claude acp prompt passes", "claude", "acp", modePrompt, 0, "", ""},
		{"claude acp steer passes", "claude", "acp", modeSteer, 0, "", ""},
		{"codex acp cancel passes", "codex", "acp", opCancel, 0, "", ""},
		// A known (harness, acp) pair whose harness has no ACP mode is a
		// contradiction, refused explicitly — mode alone grants nothing.
		{"pi acp prompt refused", "pi", "acp", modePrompt,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "no ACP drive mode"},
		{"pi acp steer refused", "pi", "acp", modeSteer,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "no ACP drive mode"},
		{"shell acp prompt refused", "shell", "acp", modePrompt,
			http.StatusUnprocessableEntity, codeUnsupportedMode, "no ACP drive mode"},
		// Unknown adapters defer in either mode: the daemon must not invent
		// a verdict for an adapter it cannot see.
		{"unknown adapter passes to the runner", "mystery", "terminal", modePrompt, 0, "", ""},
		{"unknown adapter defers in acp mode too", "mystery", "acp", modePrompt, 0, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, code, msg := semanticModeRefusal(adapters.FindByAdapter(tc.adapter), tc.adapter, tc.driveMode, tc.action)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("verdict = (%d, %q, %q), want (%d, %q)", status, code, msg, tc.wantStatus, tc.wantCode)
			}
			if tc.wantIn != "" && !strings.Contains(msg, tc.wantIn) {
				t.Fatalf("message %q missing %q", msg, tc.wantIn)
			}
			if status != 0 && !strings.Contains(msg, tc.adapter) {
				t.Fatalf("message %q does not name the harness %q", msg, tc.adapter)
			}
		})
	}
}

// The boundary is checked BEFORE residency (ADR 0033 §3): a prompt against a
// dead terminal claude session must be refused without resuming — without
// the gate, transparent resume would spawn a real interactive Claude TUI
// just to be refused by its runner one round-trip later.
func TestPromptTerminalClaudeRefusedBeforeResume(t *testing.T) {
	exited := centralstore.UnixMillis(2)
	h := newAgentHarness(t, centralstore.NewSession{
		ID: "c", Adapter: "claude", CWD: "/", Command: []string{"claude"}, ExitedAt: &exited,
	})
	h.setLive(func(centralstore.SessionID) (sessioncoord.Runtime, bool) { return sessioncoord.Runtime{}, false })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/c/prompt",
		strings.NewReader(`{"prompt":"hi","mode":"prompt"}`))
	req.Header.Set("Content-Type", "application/json")
	handleAgentPromptCentral(rec, req, h.deps(), "c")

	got := parseRecorded(t, rec)
	if got.code != http.StatusUnprocessableEntity || got.errCode() != codeUnsupportedMode {
		t.Fatalf("code=%d body=%v, want 422/%s", got.code, got.body, codeUnsupportedMode)
	}
	select {
	case id := <-h.resumes:
		t.Fatalf("refusal resumed session %s; the capability boundary must precede residency", id)
	default:
	}
	if h.subs.Load() != 0 {
		t.Fatal("refusal subscribed to outcomes; the boundary must precede the delivery machinery")
	}
}

// Cancel against a live terminal codex session is refused at the same
// boundary, before the runner is consulted at all.
func TestCancelTerminalCodexRefusedAtModeBoundary(t *testing.T) {
	h := newAgentHarness(t, centralstore.NewSession{
		ID: "x", Adapter: "codex", CWD: "/", Command: []string{"codex"},
	})
	rec := httptest.NewRecorder()
	handleAgentCancelCentral(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/x/cancel", nil), h.deps(), "x")
	got := parseRecorded(t, rec)
	if got.code != http.StatusUnprocessableEntity || got.errCode() != codeUnsupportedMode {
		t.Fatalf("code=%d body=%v, want 422/%s", got.code, got.body, codeUnsupportedMode)
	}
	select {
	case ep := <-h.cancels:
		t.Fatalf("refusal reached the runner at %s", ep)
	case <-time.After(10 * time.Millisecond):
	}
}

// PTY surfaces against an ACP-mode session answer no_terminal, naming the
// surfaces that do exist. These sessions have no PTY at all (ADR 0033:
// "n/a", not "refused"), so raw input, attach, and scrollback must not
// reach a runner or a broker.
func TestPTYSurfacesRefusedOnACPSession(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, err := st.InsertSession(ctx, centralstore.NewSession{
		ID: "a1", Adapter: "claude", DriveMode: "acp", CWD: "/", Command: []string{}, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	boot := &Bootstrap{Store: st, Registry: sessioncoord.NewRegistry()}
	fanout := newSSEFanout()
	dirs := sessionmeta.New(t.TempDir())

	for _, tc := range []struct {
		name string
		req  *http.Request
		want string // substring naming the working surface
	}{
		{"input", httptest.NewRequest(http.MethodPost, "/v1/sessions/a1/input", strings.NewReader("keys")), "gmux agent prompt"},
		{"attach", httptest.NewRequest(http.MethodPost, "/v1/sessions/a1/attach", nil), "conversation"},
		{"scrollback", httptest.NewRequest(http.MethodGet, "/v1/sessions/a1/scrollback", nil), "gmux agent logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleCentralSessionAction(rec, tc.req, boot, fanout, &wire.Converter{}, nil, dirs, "/usr/bin/gmux", nil)
			got := parseRecorded(t, rec)
			if got.code != http.StatusUnprocessableEntity || got.errCode() != codeNoTerminal {
				t.Fatalf("code=%d body=%v, want 422/%s", got.code, got.body, codeNoTerminal)
			}
			if msg := got.errMessage(); !strings.Contains(msg, "ACP session") || !strings.Contains(msg, tc.want) {
				t.Fatalf("message %q must name the boundary and point at %q", msg, tc.want)
			}
		})
	}
}

// /ws/{id} regression: the terminal-WebSocket backend must be refused for an
// ACP session from the AUTHORITATIVE store, even when the composed fanout
// snapshot is empty/stale and the live registry holds an endpoint —
// exactly the state immediately after an ACP registration. The old
// snapshot-only check fell through to the registry and attempted a
// terminal proxy against a runner that has no PTY.
func TestTerminalWSEndpointRefusesACPSessionFromStore(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sock := filepath.Join(t.TempDir(), "acp-runner.sock")
	runners := &bootstrapRunners{
		metas: map[string]sessioncoord.RunnerMeta{sock: {PID: 77, Incarnation: "inc-acp", Registration: centralstore.RunnerRegistration{
			ID: "w1acp000", Adapter: "claude", DriveMode: "acp", Alive: true, CreatedAt: 1, ObservedAt: 1,
		}}},
		blocked: map[string]bool{},
	}
	reg := sessioncoord.NewRegistry()
	coord := sessioncoord.New(reg, runners, st, nil, nil)
	t.Cleanup(coord.Close)
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: sock}); err != nil {
		t.Fatal(err)
	}
	if _, live := registryRuntime(reg, "w1acp000"); !live {
		t.Fatal("test premise broken: registry must hold a live endpoint")
	}

	// Empty fanout: the snapshot has not composed this session yet.
	endpoint, err := terminalWSEndpoint(ctx, st, reg, newSSEFanout(), "w1acp000")
	if err == nil || endpoint != "" {
		t.Fatalf("resolved %q, %v; want ACP refusal", endpoint, err)
	}
	if !strings.Contains(err.Error(), "ACP session") {
		t.Fatalf("error %q must name the ACP boundary", err)
	}

	// A terminal session with a live endpoint still resolves.
	sock2 := filepath.Join(t.TempDir(), "pty-runner.sock")
	runners.metas[sock2] = sessioncoord.RunnerMeta{PID: 78, Incarnation: "inc-pty", Registration: centralstore.RunnerRegistration{
		ID: "w2pty000", Adapter: "pi", Alive: true, CreatedAt: 1, ObservedAt: 1,
	}}
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: sock2}); err != nil {
		t.Fatal(err)
	}
	endpoint, err = terminalWSEndpoint(ctx, st, reg, newSSEFanout(), "w2pty000")
	if err != nil || endpoint != sock2 {
		t.Fatalf("resolved %q, %v; want %q", endpoint, err, sock2)
	}
}

// A store failure must refuse the terminal-WS backend conservatively: no
// endpoint may be resolved when the session's mode cannot be established.
func TestTerminalWSEndpointRefusesOnStoreFailure(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close() // every read now fails
	endpoint, err := terminalWSEndpoint(ctx, st, sessioncoord.NewRegistry(), newSSEFanout(), "s1")
	if err == nil || endpoint != "" {
		t.Fatalf("resolved %q, %v; want conservative refusal", endpoint, err)
	}
	if !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("error %q must state the mode could not be verified", err)
	}
}

// Raw input must not proceed to a live endpoint when the store cannot
// establish the session's mode: a store failure is an internal error, not a
// bypass of the safety boundary.
func TestInputRefusesOnStoreFailure(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertSession(ctx, centralstore.NewSession{
		ID: "b1", Adapter: "claude", DriveMode: "acp", CWD: "/", Command: []string{}, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close() // every read now fails
	boot := &Bootstrap{Store: st, Registry: sessioncoord.NewRegistry()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/b1/input", strings.NewReader("keys"))
	handleCentralSessionAction(rec, req, boot, newSSEFanout(), &wire.Converter{}, nil, sessionmeta.New(t.TempDir()), "/usr/bin/gmux", nil)
	got := parseRecorded(t, rec)
	if got.code != http.StatusInternalServerError || got.errCode() != "internal" {
		t.Fatalf("code=%d body=%v, want 500/internal", got.code, got.body)
	}
}
