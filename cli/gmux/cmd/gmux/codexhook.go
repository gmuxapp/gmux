package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// codexHook implements the hidden `gmux __codex-hook <Event>` subcommand that
// codex invokes as a command hook (injected per-launch by the codex adapter
// via `-c hooks.<Event>=...` config overrides). codex writes the event payload
// as JSON to this
// process's stdin; we translate it to the tool-neutral /hook/event protocol
// (docs/runner-hook-protocol.md) and POST it to the runner socket named by
// GMUX_SESSION_SOCK.
//
// It is fire-and-forget and must never break codex: it always exits 0 and
// always prints "{}" on stdout (the minimal valid JSON the codex Stop hook
// requires). When GMUX_SESSION_SOCK is unset the codex run was not launched by
// gmux, so the hook is a no-op — this is why the globally-installed hook is
// safe for plain `codex` invocations outside gmux.
func codexHook(eventName string) int {
	// Always read stdin (codex pipes the event JSON in and may block on the
	// write if we don't drain it), bounded so a pathological payload can't
	// exhaust memory.
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))

	if sock := os.Getenv("GMUX_SESSION_SOCK"); sock != "" {
		for _, body := range adapters.CodexHookBodies(eventName, input) {
			postCodexHookEvent(sock, body)
		}
	}

	fmt.Println("{}")
	return 0
}

// postCodexHookEvent POSTs one event body to the runner's /hook/event over the
// Unix socket. Best-effort with a short timeout; transport errors are swallowed
// so the hook never surfaces a failure into codex.
func postCodexHookEvent(sockPath string, body []byte) {
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	resp, err := client.Post("http://unix/hook/event", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}
