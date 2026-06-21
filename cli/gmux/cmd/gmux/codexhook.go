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
	runCodexHook(eventName, os.Stdin, os.Stdout, os.Getenv("GMUX_SESSION_SOCK"))
	return 0
}

// runCodexHook is the testable core of the subcommand. sock is the runner
// socket (GMUX_SESSION_SOCK); when empty the codex run was not launched by gmux
// and the hook is a pure no-op apart from emitting the obligatory "{}" — this is
// what keeps the per-launch-injected hook inert for plain `codex` invocations.
func runCodexHook(eventName string, in io.Reader, out io.Writer, sock string) {
	// Always drain stdin (codex pipes the event JSON in and can block on the
	// write if we don't read it), bounded so a pathological payload can't
	// exhaust memory.
	input, _ := io.ReadAll(io.LimitReader(in, 1<<20))

	if sock != "" {
		for _, body := range adapters.CodexHookBodies(eventName, input) {
			postCodexHookEvent(sock, body)
		}
	}

	fmt.Fprintln(out, "{}")
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
