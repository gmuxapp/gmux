package main

// `gmuxd state check|backup|export` — admin state tooling (cutover design
// §5). The canonical front is `gmux daemon state ...`, which forwards here
// (mirroring start/stop/status routing, ADR 0009).
//
// Online mode talks to the running daemon over the Unix socket
// (/v1/state/*). When no daemon is reachable, check and backup fall back to
// offline mode behind statetool's ownership gate (advisory lock file +
// health heuristic + held BEGIN IMMEDIATE); export requires a running
// daemon.
//
// Exit codes: 0 success (check: no findings), 1 check found findings,
// 2 usage error, 3 the operation could not run.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

var (
	stateCheckTimeout  = 30 * time.Second
	stateBackupTimeout = 5 * time.Minute
)

const stateUsage = `Usage: gmuxd state <command>

Commands:
  check              Verify database integrity and domain invariants
  backup <path>      Write a consistent backup of the database (VACUUM INTO)
  export             Print a redacted JSON inventory of the daemon state

check and backup prefer the running daemon; with no daemon running they
operate on the database directly (refusing when it appears owned). export
requires a running daemon. Backups CONTAIN PEER TOKENS — keep them private.
An online backup briefly serializes against all daemon work.

Exit codes: 0 ok, 1 check found problems, 2 usage error, 3 could not run.
`

func runState(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, stateUsage)
		return 2
	}
	sub := args[0]
	rest := args[1:]
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			_, _ = fmt.Fprint(stdout, stateUsage)
			return 0
		}
	}
	switch sub {
	case "check":
		if len(rest) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd state check: unexpected arguments\n")
			return 2
		}
		return runStateCheck(stdout, stderr)
	case "backup":
		if len(rest) != 1 {
			_, _ = fmt.Fprintf(stderr, "gmuxd state backup: exactly one target path required\n")
			return 2
		}
		return runStateBackup(rest[0], stdout, stderr)
	case "export":
		if len(rest) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd state export: unexpected arguments\n")
			return 2
		}
		return runStateExport(stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "gmuxd state: unknown command %q\n\n%s", sub, stateUsage)
		return 2
	}
}

// stateEnvelope mirrors statetool's response wrapper.
type stateEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// callState performs one request against the daemon socket. reachable is
// false only when no daemon answered at all (connection error) — the signal
// to try offline mode. Any HTTP answer means a daemon is running and owns
// (or will own) the database, so offline mode must not be attempted.
func callState(method, path string, body []byte, timeout time.Duration) (env stateEnvelope, status int, reachable, absent bool, transportErr error) {
	client := unixipc.Client(paths.SocketPath())
	client.Timeout = timeout
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://localhost"+path, reqBody)
	if err != nil {
		return env, 0, false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// Offline fallback is safe only when the socket genuinely does not
		// exist or no process is listening. Timeouts, deadline expiry,
		// accepted-then-reset connections, and all other socket/HTTP
		// failures can come from a live owner and must fail closed.
		missing := errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
		return env, 0, false, missing, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return env, resp.StatusCode, true, false, err
	}
	_ = json.Unmarshal(raw, &env) // non-JSON (e.g. mux 404 text) leaves the zero envelope
	return env, resp.StatusCode, true, false, nil
}

// explainOnlineFailure renders the daemon's refusal. A 404 means a daemon
// without the state routes (pre-cutover); a 503 central_store_not_active
// means the routes exist but no central store is open. Both are "stop the
// daemon or upgrade" situations, never an offline fallback (the daemon is
// alive and owns the state directory).
func explainOnlineFailure(op string, env stateEnvelope, status int, stderr io.Writer) int {
	switch {
	case status == http.StatusNotFound:
		_, _ = fmt.Fprintf(stderr, "gmuxd state %s: the running daemon does not serve state routes (pre-cutover daemon); stop it to run offline, or upgrade it\n", op)
	case env.Error.Code == "central_store_not_active":
		_, _ = fmt.Fprintf(stderr, "gmuxd state %s: the running daemon has no central store active; stop it to run offline\n", op)
	case env.Error.Message != "":
		_, _ = fmt.Fprintf(stderr, "gmuxd state %s: %s\n", op, env.Error.Message)
	default:
		_, _ = fmt.Fprintf(stderr, "gmuxd state %s: daemon answered HTTP %d\n", op, status)
	}
	return 3
}

func printCheckReport(report statetool.CheckReport, mode string, stdout io.Writer) int {
	if report.OK {
		_, _ = fmt.Fprintf(stdout, "state check (%s): ok\n", mode)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "state check (%s): %d finding(s)\n", mode, len(report.Findings))
	for _, f := range report.Findings {
		_, _ = fmt.Fprintf(stdout, "  [%s] %s\n", f.Code, f.Message)
	}
	return 1
}

func runStateCheck(stdout, stderr io.Writer) int {
	env, status, reachable, absent, transportErr := callState(http.MethodGet, "/v1/state/check", nil, stateCheckTimeout)
	if reachable {
		if status != http.StatusOK || !env.OK {
			return explainOnlineFailure("check", env, status, stderr)
		}
		var report statetool.CheckReport
		if err := json.Unmarshal(env.Data, &report); err != nil {
			_, _ = fmt.Fprintf(stderr, "gmuxd state check: malformed daemon response: %v\n", err)
			return 3
		}
		return printCheckReport(report, "online", stdout)
	}
	if !absent {
		_, _ = fmt.Fprintf(stderr, "gmuxd state check: daemon did not answer (%v); not falling back to offline mode\n", transportErr)
		return 3
	}
	// The socket is genuinely absent/refused: offline mode behind the ownership gate.
	return withOfflineStore("check", stderr, func(ctx context.Context, store *centralstore.Store) int {
		findings, err := store.CheckState(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "gmuxd state check: %v\n", err)
			return 3
		}
		return printCheckReport(statetool.ReportFor(findings), "offline", stdout)
	})
}

func runStateBackup(target string, stdout, stderr io.Writer) int {
	absolute, err := filepath.Abs(target)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd state backup: %v\n", err)
		return 3
	}
	body, _ := json.Marshal(map[string]string{"path": absolute})
	// Generous timeout: an online backup serializes against all daemon
	// work for its duration (design §5).
	env, status, reachable, absent, transportErr := callState(http.MethodPost, "/v1/state/backup", body, stateBackupTimeout)
	if reachable {
		if status != http.StatusOK || !env.OK {
			return explainOnlineFailure("backup", env, status, stderr)
		}
		_, _ = fmt.Fprintf(stdout, "backup written: %s\nnote: %s\n", absolute, statetool.BackupNote)
		return 0
	}
	if !absent {
		_, _ = fmt.Fprintf(stderr, "gmuxd state backup: daemon did not answer (%v); not falling back to offline mode\n", transportErr)
		return 3
	}
	return withOfflineStore("backup", stderr, func(ctx context.Context, store *centralstore.Store) int {
		if err := store.BackupInto(ctx, absolute); err != nil {
			_, _ = fmt.Fprintf(stderr, "gmuxd state backup: %v\n", err)
			return 3
		}
		_, _ = fmt.Fprintf(stdout, "backup written (offline): %s\nnote: %s\n", absolute, statetool.BackupNote)
		return 0
	})
}

func runStateExport(stdout, stderr io.Writer) int {
	env, status, reachable, _, _ := callState(http.MethodGet, "/v1/state/export", nil, 30*time.Second)
	if !reachable {
		_, _ = fmt.Fprintf(stderr, "gmuxd state export: no daemon is running (export requires a running daemon)\n")
		return 3
	}
	if status != http.StatusOK || !env.OK {
		return explainOnlineFailure("export", env, status, stderr)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, env.Data, "", "  "); err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd state export: malformed daemon response: %v\n", err)
		return 3
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", indented.Bytes())
	return 0
}

// withOfflineStore acquires the offline ownership gate, opens the database
// read-only, runs op, and releases everything.
func withOfflineStore(op string, stderr io.Writer, fn func(context.Context, *centralstore.Store) int) int {
	ctx := context.Background()
	sock := paths.SocketPath()
	handle, err := statetool.OpenOffline(ctx, paths.StateDir(), func() bool { return unixipc.Healthy(sock) })
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd state %s: offline mode unavailable: %v\n", op, err)
		return 3
	}
	defer handle.Close()
	store, err := centralstore.OpenReadOnly(ctx, handle.StateDir())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd state %s: %v\n", op, err)
		return 3
	}
	defer store.Close()
	return fn(ctx, store)
}
