package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// claudeHook implements the hidden `gmux __claude-hook` subcommand that Claude
// Code invokes as a command hook (injected per-launch by the claude adapter's
// HookCommand via `--settings`). Claude writes the event payload as JSON to
// stdin (carrying hook_event_name); we translate it to the tool-neutral
// /hook/event protocol (docs/runner-hook-protocol.md) and POST it to the runner
// socket named by GMUX_SESSION_SOCK.
//
// Fire-and-forget and always exits 0: a hook must never block or fail Claude.
// When GMUX_SESSION_SOCK is unset the run was not launched by gmux, so the hook
// is a no-op. It writes nothing to stdout — Claude folds a hook's stdout into
// the model context for some events (UserPromptSubmit), so emitting anything
// would pollute the conversation.
func claudeHook() int {
	runClaudeHook(os.Stdin, os.Getenv("GMUX_SESSION_SOCK"))
	return 0
}

// runClaudeHook is the testable core. sock is the runner socket
// (GMUX_SESSION_SOCK); when empty the hook only drains stdin and returns.
func runClaudeHook(in io.Reader, sock string) {
	// Drain stdin (Claude pipes the event JSON and can block on the write if we
	// don't read it), bounded so a pathological payload can't exhaust memory.
	input, _ := io.ReadAll(io.LimitReader(in, 1<<20))
	if sock == "" {
		return
	}
	var event struct {
		Name           string `json:"hook_event_name"`
		TranscriptPath string `json:"transcript_path"`
	}
	_ = json.Unmarshal(input, &event)
	if event.Name == "Stop" || event.Name == "StopFailure" {
		awaitClaudeTerminalExchange(event.TranscriptPath, 500*time.Millisecond)
	}
	for _, body := range adapters.ClaudeHookBodies(input) {
		postClaudeHookEvent(sock, body)
	}
	if event.Name == "SessionStart" {
		scheduleClaudeReady(sock)
	}
}

// Claude invokes SessionStart before its interactive composer can reliably
// retain terminal input. A detached helper reports readiness after that startup
// window; keeping the delay out of the awaited hook lets Claude finish mounting
// its UI. Duplicate SessionStart events are harmless because runner readiness
// is idempotent.
var scheduleClaudeReady = func(sock string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "__claude-ready")
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sock)
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

const claudeComposerStartupDelay = 2 * time.Second

func claudeReady() int {
	runClaudeReady(os.Getenv("GMUX_SESSION_SOCK"), claudeComposerStartupDelay)
	return 0
}

func runClaudeReady(sock string, delay time.Duration) {
	if sock == "" {
		return
	}
	time.Sleep(delay)
	postClaudeHookEvent(sock, []byte(`{"op":"ready"}`))
}

// postClaudeHookEvent POSTs one event body to the runner's /hook/event over the
// Unix socket. Best-effort with a short timeout; transport errors are swallowed
// so the hook never surfaces a failure into Claude.
// Claude can invoke its terminal hook just before the final transcript append
// becomes visible. Give that append a short bounded window before publishing
// the turn end, so an observational wait can render the exchange it resolved.
func awaitClaudeTerminalExchange(ref string, timeout time.Duration) {
	if ref == "" {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		exchanges, err := adapters.NewClaude().RenderConversationExchanges(ref)
		if err == nil && len(exchanges) > 0 && exchanges[len(exchanges)-1].Iterations > 0 {
			// The runner bind and conversation watcher reach gmuxd on separate
			// streams. Let the already-visible append propagate before the turn
			// end releases observational waiters.
			time.Sleep(200 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func postClaudeHookEvent(sock string, body []byte) {
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}
	resp, err := client.Post("http://unix/hook/event", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}
