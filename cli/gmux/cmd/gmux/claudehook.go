package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
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
	for _, body := range adapters.ClaudeHookBodies(input) {
		postClaudeHookEvent(sock, body)
	}
}

// postClaudeHookEvent POSTs one event body to the runner's /hook/event over the
// Unix socket. Best-effort with a short timeout; transport errors are swallowed
// so the hook never surfaces a failure into Claude.
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
