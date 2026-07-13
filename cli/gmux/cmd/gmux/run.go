package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/binhash"
	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
	"github.com/gmuxapp/gmux/cli/gmux/internal/naming"
	"github.com/gmuxapp/gmux/cli/gmux/internal/ptyserver"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/packages/workspace"
)

// handshakeTimeout bounds how long the parent end of spawnDetached
// waits for the child to finish registering with gmuxd. The child's
// registerWithGmuxd loops up to 5 times with 500ms backoff, so the
// realistic worst case is ~2.5s plus child startup. 5s is a
// comfortable ceiling: any longer and something is genuinely wrong.
const handshakeTimeout = 5 * time.Second

// ptyDrainTimeout bounds the wait for the PTY to fully flush after the
// child exits. A well-behaved child has its PTY slave closed by the
// kernel as soon as it exits, so EOF on ptmx arrives almost immediately
// and this timeout is never approached in practice. The ceiling only
// matters when a grandchild is still holding the slave open: we'd
// rather restore the user's terminal promptly than wait forever on a
// background writer.
const ptyDrainTimeout = 250 * time.Millisecond

// runDirectives carries daemon→runner overrides for a /v1/launch,
// /v1/resume, or /v1/restart. End-user invocations leave all three
// fields zero. See the flag declarations in cli.go for semantics.
type runDirectives struct {
	ResumeID    string
	InitialCols int
	InitialRows int

	// ForceForeground disables the nested-gmux auto-detach: even when
	// running inside an existing gmux session, attach the local terminal
	// and block until the child exits. Required by callers with blocking
	// semantics (`gmux edit` as $EDITOR must not return before the user
	// closes the file), where detaching would silently return exit code 0
	// to the invoking program (git would commit an unedited message).
	ForceForeground bool

	// ParentSessionID records the session this one was spawned from
	// (e.g. `gmux edit` invoked as $EDITOR inside an existing session).
	// Flows into session meta so the UI can relate the two.
	ParentSessionID string
}

// reapOnFatalRegistration reports whether a runner should tear itself
// down because registration with gmuxd failed permanently.
//
// Only the combination of a fatal (4xx) verdict and a headless runner
// qualifies:
//
//   - registerFatal means gmuxd understood the request and refused it
//     for good (e.g. its IsValidSessionID guard rejected the id) —
//     retrying or waiting changes nothing. registerUnavailable, by
//     contrast, is a transient "gmuxd not ready" and must NOT reap: the
//     runner keeps serving so a later discovery scan can pick it up.
//   - !interactive means no local terminal is attached, so gmuxd is the
//     only consumer; once gmuxd has permanently rejected it, the
//     process is a pure orphan (the convIndex-rehydrate resume bug,
//     where a session keyed by its conversation UUID could never
//     register). An interactive runner is spared: the user's terminal
//     is still usefully attached even if gmux never tracks the session.
func reapOnFatalRegistration(outcome registerOutcome, interactive bool) bool {
	return outcome == registerFatal && !interactive
}

// runSession launches a new managed session for the given command.
//
// When attach is true and stdin is a tty, the local terminal is wired
// to the PTY so the command behaves transparently (the default). When
// attach is false, the session is spawned detached from the tty and
// this call returns immediately once the session is running, leaving
// the session visible in the gmux UI.
func runSession(args []string, attach bool, dir runDirectives) {
	// Resolve the adapter up front so we can short-circuit one-shot, non-session
	// invocations (e.g. `pi update`, `pi list`) before any session machinery.
	// These are not interactive sessions: exec them directly so they behave
	// exactly as if typed by hand (same tty, env, exit code) and never get
	// wrapped in a runner/PTY or registered with gmuxd. This must run before the
	// nested/detach branches below — detaching or re-execing a one-shot command
	// would be wrong (and would hang -d on a registration handshake that never
	// comes).
	a := resolveAdapter(args)
	if pt, ok := a.(adapter.PassthroughDetector); ok && pt.IsPassthrough(args) {
		execPassthrough(args)
	}

	// Nested gmux detection: if we're running interactively inside an
	// existing gmux session, re-exec as a detached headless process instead
	// of doing PTY passthrough (which would nest PTY-within-PTY). The
	// detached process registers with gmuxd and the session appears in the
	// gmux UI. The original process returns immediately to the parent shell.
	if os.Getenv("GMUX") == "1" && localterm.IsInteractive() && !dir.ForceForeground {
		spawnDetached(args, "started "+strings.Join(args, " ")+" in background (visible in gmux)", false)
		return
	}

	// Explicit -d/--detach: spawn detached, wait for the child to finish
	// registering with gmuxd, then print the session id on stdout so the
	// caller (typically a script) can capture it for tail / kill
	// without polling.
	if !attach {
		spawnDetached(args, "", true)
		return
	}

	// Honour the legacy GMUX_RESUME_ID env var when --resume-id
	// wasn't provided. Older gmuxd builds set it via env; newer
	// gmuxd sets the flag instead. The env-var path is kept for
	// rolling-upgrade scenarios (a daemon installed before the
	// flag existed talking to a newer runner) and will be removed
	// in a future release.
	if dir.ResumeID == "" {
		dir.ResumeID = os.Getenv("GMUX_RESUME_ID")
	}

	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine cwd: %v", err)
	}

	// --resume-id, when the daemon passed it on /resume or
	// /restart, makes the runner keep the existing session id
	// across the seam (including its scrollback directory on
	// disk; see ADR 0003). Without it we generate a fresh id, so
	// nested `gmux foo` invocations inside a session don't try to
	// re-bind the parent's id.
	sessionID := dir.ResumeID
	if sessionID == "" {
		sessionID = naming.SessionID()
	}
	socketDir := paths.SessionSocketDir()
	sockPath := filepath.Join(socketDir, sessionID+".sock")

	// Bind the socket BEFORE any sessionID-dependent setup
	// (scrollback path, env, state). On collision with a live
	// runner — which typically only happens when a daemon-supplied
	// GMUX_RESUME_ID lands in a window where the targeted session
	// is actually still alive — fall back to a fresh id and bind
	// that instead. See ADR 0003 "Collision handling".
	listener, err := ptyserver.BindSocket(sockPath)
	if errors.Is(err, ptyserver.ErrSocketInUse) {
		log.Printf("gmux: requested session id %s is in use; falling back to a fresh id", sessionID)
		sessionID = naming.SessionID()
		sockPath = filepath.Join(socketDir, sessionID+".sock")
		listener, err = ptyserver.BindSocket(sockPath)
	}
	if err != nil {
		log.Fatalf("failed to bind session socket: %v", err)
	}

	// Get adapter-specific env vars
	adapterEnv := a.Env(adapter.EnvContext{
		Cwd:        workDir,
		SessionID:  sessionID,
		SocketPath: sockPath,
	})

	// Detect VCS workspace root and remotes for grouping related sessions.
	wsRoot := workspace.DetectRoot(workDir)
	remotes := workspace.DetectRemotes(wsRoot)

	// Create in-memory session state
	state := session.New(session.Config{
		ID:              sessionID,
		Command:         args,
		Cwd:             workDir,
		Adapter:         a.Name(),
		ParentSessionID: dir.ParentSessionID,
		WorkspaceRoot:   wsRoot,
		Remotes:         remotes,
		SocketPath:      sockPath,
		BinaryHash:      binhash.Self(),
		RunnerVersion:   version,
	})

	// Common env vars — set for every child, per ADR-0005
	env := []string{
		"GMUX=1",
		"GMUX_SOCKET=" + sockPath,
		"GMUX_SESSION_ID=" + sessionID,
		"GMUX_ADAPTER=" + a.Name(),
		"GMUX_RUNNER_VERSION=" + version,
	}
	env = append(env, adapterEnv...)
	env = append(env, sessionEditorEnv(os.LookupEnv, os.Executable)...)

	interactive := localterm.IsInteractive()

	// Open the persistent scrollback sink for this runner. Best-
	// effort: a failure to open (disk full, permission denied)
	// just leaves the sink off, the runner serves live data
	// normally, and dead-session replay won't have anything to
	// show. The active file lives at
	// $XDG_STATE_HOME/gmux/sessions/<id>/scrollback, in the same
	// per-session directory gmuxd's sessionmeta package writes
	// meta.json into.
	scrollbackPath := filepath.Join(paths.SessionDir(sessionID), scrollback.ActiveName)

	// Determine initial PTY size — use terminal size if interactive
	ptyCfg := ptyserver.Config{
		Command:    args,
		Cwd:        workDir,
		Env:        env,
		Listener:   listener,
		SocketPath: sockPath,
		Adapter:    a,
		State:      state,
		Version:    version,
	}
	// Conditional assignment: a typed nil *scrollback.Writer
	// stored in ptyCfg.Scrollback (an io.WriteCloser) would
	// satisfy != nil checks downstream and panic on the first
	// Write. Only assign on successful Open.
	if sw, err := scrollback.Open(scrollbackPath); err != nil {
		log.Printf("scrollback: %v (continuing without persistence)", err)
	} else {
		ptyCfg.Scrollback = sw
	}
	// Always try to inherit terminal dimensions from the parent.
	// Even non-interactive launches (background, piped) benefit from
	// a real size: the PTY and virtual terminal start correctly sized
	// instead of falling back to 80x24.
	if cols, rows, err := localterm.TerminalSize(); err == nil {
		ptyCfg.Cols = cols
		ptyCfg.Rows = rows
	}
	// --initial-cols / --initial-rows, when the daemon passed
	// them on /resume or /restart, override the local TTY
	// detection: a detached runner has no TTY to read from, and
	// the daemon knows the last-attached browser's dimensions
	// from its store. Without this, the PTY would start at 80x24
	// and any child process that captures $COLUMNS at startup
	// (claude, prompt frameworks, etc.) would render at the
	// default width until the next manual resize.
	if dir.InitialCols > 0 {
		ptyCfg.Cols = uint16(dir.InitialCols)
	}
	if dir.InitialRows > 0 {
		ptyCfg.Rows = uint16(dir.InitialRows)
	}

	// In interactive mode, build the local terminal attach before
	// starting the PTY server so LocalOut is wired from the very first
	// read. Otherwise a fast-exiting command could have its entire
	// output flushed before SetLocalOutput is called and be silently
	// dropped on the floor.
	//
	// The PTYWriter and ResizeFn closures read `srv` by reference; the
	// variable is assigned by the ptyserver.New call below, and
	// attach.Start() — which is what actually starts the goroutines
	// that invoke these closures — is deferred until after that point.
	var (
		srv      *ptyserver.Server
		localTty *localterm.Attach
	)
	if interactive {
		lt, err := localterm.New(localterm.Config{
			PTYWriter: ptyWriterFunc(func(p []byte) (int, error) {
				return srv.WritePTY(p)
			}),
			ResizeFn: func(cols, rows uint16) { srv.Resize(cols, rows) },
		})
		if err != nil {
			log.Fatalf("failed to attach terminal: %v", err)
		}
		localTty = lt
		ptyCfg.LocalOut = localTty
	}

	if !interactive {
		fmt.Printf("session:  %s\n", sessionID)
		fmt.Printf("adapter:  %s\n", a.Name())
		fmt.Printf("command:  %s\n", strings.Join(args, " "))
	}

	// Start PTY server. The socket is already bound to `listener`
	// (above); ptyserver.New takes ownership and serves on it.
	srv, err = ptyserver.New(ptyCfg)
	if err != nil {
		if localTty != nil {
			// Restore cooked mode before fataling out; otherwise the
			// user's terminal is left in raw mode on the shell prompt.
			localTty.Detach()
		}
		log.Fatalf("failed to start: %v", err)
	}

	state.SetRunning(srv.Pid())

	// Default turn model (non-hook-driven adapters): the session is
	// active from launch. Prompt marks — if the child's shell
	// integration emits them — upgrade it to per-command turns inside
	// the ptyserver; otherwise this one lifetime-long turn is closed by
	// the exit handling below. Agent adapters skip this: their hooks own
	// Working, and a launch-time true would misreport an idle agent.
	if !adapter.HookDriven(a) {
		state.SetStatus(&adapter.Status{Working: true})
	}

	if !interactive {
		fmt.Printf("pid:      %d\n", srv.Pid())
		fmt.Printf("socket:   %s\n", srv.SocketPath())
		fmt.Println("serving...")
	}

	// Auto-start gmuxd if not running (one-shot, never retried), then
	// register. The goroutine signals regDone when the registration
	// HTTP call has completed (succeeded or exhausted retries) and the
	// handshake — if any — has been delivered to the parent. We block
	// on regDone before exit so a fast-exiting command (echo, true,
	// false) can't lose the registration race.
	ensureGmuxd()
	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		outcome := registerWithGmuxd(sessionID, sockPath)
		handshakeAck(sessionID, outcome.ok())
		if reapOnFatalRegistration(outcome, interactive) {
			log.Printf("gmux: registration permanently rejected for %s; shutting down orphaned runner", sessionID)
			srv.Shutdown()
		}
	}()

	if interactive {
		// Transparent mode: the local tty was built above and wired as
		// ptyCfg.LocalOut; now that srv exists, kick off the stdin/winch
		// relay goroutines that call back into srv.
		localTty.Start()

		// In interactive mode:
		// - SIGHUP → detach local terminal, keep session running
		// - SIGINT/SIGTERM are consumed by raw mode and forwarded to child via PTY
		//   (but we still catch them on gmux in case raw mode is somehow bypassed)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-srv.Done():
			// Child exited. Wait (briefly, bounded) for the final PTY
			// flush so trailing output reaches the user's terminal
			// before we detach and restore cooked mode — otherwise the
			// coalesce buffer's last chunk can land on an already-
			// detached Attach and be silently dropped.
			select {
			case <-srv.PTYDone():
			case <-time.After(ptyDrainTimeout):
			}
			localTty.Detach()
		case <-localTty.Done():
			// Local terminal gone (stdin closed) — session continues headless
			srv.SetLocalOutput(nil)
			// Wait for child to exit (session persists, accessible via web UI)
			<-srv.Done()
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				// Terminal closed — detach, keep session alive
				localTty.Detach()
				srv.SetLocalOutput(nil)
				// Continue running headless until child exits
				<-srv.Done()
			} else {
				// SIGINT/SIGTERM — clean shutdown
				localTty.Detach()
				srv.Shutdown()
			}
		}
	} else {
		// Non-interactive: original behavior
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-srv.Done():
			// Child exited
		case sig := <-sigCh:
			fmt.Printf("\nreceived %v, shutting down...\n", sig)
			srv.Shutdown()
		}
	}

	exitCode := srv.ExitCode()

	// Wait for the final PTY flush before reading LifetimeTurnOpen, so a
	// prompt mark in the child's last bytes still counts (bounded;
	// idempotent with the interactive path's earlier drain).
	select {
	case <-srv.PTYDone():
	case <-time.After(ptyDrainTimeout):
	}
	finalizeSessionState(state, srv.LifetimeTurnOpen(), exitCode)

	// Wait for the register/handshake goroutine to finish before we
	// touch deregister or exit. Otherwise a fast-exiting command
	// races with its own registration: the child can deregister
	// (no-op, not registered yet), then the register arrives, then
	// the child exits, leaving a stale registered session.
	//
	// Bounded by registerWithGmuxd's retry budget (≤2.5s).
	<-regDone

	// Deregister from gmuxd (best-effort)
	deregisterFromGmuxd(sessionID)

	if !interactive {
		fmt.Printf("exited:   %d\n", exitCode)
	}
	os.Exit(exitCode)
}

// finalizeSessionState records the child's exit on the session state,
// closing the lifetime turn first when it is still open. For sessions
// that never emitted prompt marks the exit IS the turn end (`gmux --
// pnpm test`): emit idle (+error on a non-zero exit code) and flag
// unread, so waits resolve as "idle" and the sidebar shows "waiting on
// you" — exactly like an agent finishing its turn. Sessions upgraded
// to prompt-cycle turns keep their last mark-derived state: exiting at
// the prompt already reads idle, dying mid-command stays working and
// resolves as "died" (ADR 0023).
//
// The ordering is load-bearing: the turn-close status and unread
// events must be emitted before the exit event, so a subscriber (the
// daemon's wait machinery) that resolves on the first terminal signal
// it sees observes the closed turn, and the store's exit handling
// persists the final Status rather than a stale mid-turn one.
func finalizeSessionState(state *session.State, lifetimeTurnOpen bool, exitCode int) {
	if lifetimeTurnOpen {
		state.SetStatus(&adapter.Status{Working: false, Error: exitCode != 0})
		state.SetUnread(true)
	}
	state.SetExited(exitCode)
}

// sessionEditorEnv returns EDITOR/VISUAL entries pointing at `gmux
// edit` for whichever of the two the invoking environment does NOT
// already define. Inside a gmux session, programs that shell out to an
// editor (git commit, crontab -e, ...) then open the file as a managed
// editor session with zero user configuration.
//
// Default-if-unset, never override: duplicate env keys resolve
// inconsistently across consumers (glibc getenv takes the first entry,
// bash exports the last), so appending an override would be
// unreliable — and a user who exported EDITOR=vim chose vim. Shell rc
// files that export EDITOR, and git's core.editor, still outrank this
// default, as they should.
//
// The value uses the runner's own absolute binary path (selfExe), not
// a bare "gmux": dev builds and per-session runners aren't necessarily
// on the child's PATH. Falls back to "gmux" if the path is unknown.
func sessionEditorEnv(lookupEnv func(string) (string, bool), selfExe func() (string, error)) []string {
	bin := "gmux"
	if p, err := selfExe(); err == nil {
		bin = p
	}
	editCmd := bin + " edit"
	var out []string
	for _, name := range []string{"EDITOR", "VISUAL"} {
		if _, set := lookupEnv(name); !set {
			out = append(out, name+"="+editCmd)
		}
	}
	return out
}

// resolveAdapter builds the adapter registry (registered adapters first,
// shell fallback) and resolves the one matching the command.
func resolveAdapter(args []string) adapter.Adapter {
	registry := adapter.NewRegistry()
	for _, a := range adapters.All {
		registry.Register(a)
	}
	registry.SetFallback(adapters.DefaultFallback())
	return registry.Resolve(args)
}

// execPassthrough replaces the gmux process with args, so a one-shot
// invocation runs exactly as if typed directly. Never returns on success.
func execPassthrough(args []string) {
	bin, err := exec.LookPath(args[0])
	if err != nil {
		log.Fatalf("gmux: %s: %v", args[0], err)
	}
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		log.Fatalf("gmux: exec %s: %v", args[0], err)
	}
}

// reexecRunArgs builds the argv for re-execing gmux to run a command
// detached: the internal `__run -- <cmd>` form (ADR 0009). The `--`
// delivers the command verbatim even when its own args look like flags,
// and `__run` is required because the bare-command shorthand was removed
// in 2.0 (a bare `gmux <cmd>` is now an "unknown command" error).
func reexecRunArgs(args []string) []string {
	return append([]string{"__run", "--"}, args...)
}

// spawnDetached re-execs gmux with the given args as a setsid'd
// background process, disconnected from the current terminal. Used
// for both detached (-d) and nested-gmux scenarios: the child registers
// with gmuxd and appears in the UI.
//
// When waitForRegistration is true, the parent blocks until the child
// either acknowledges registration via the handshake pipe or fails;
// on success it prints the session id on stdout, on failure it exits
// non-zero with a stderr error. This is the detached (-d) path: scripts
// capture the id with id=$(gmux -d -- foo) and use it
// immediately, without polling.
//
// When waitForRegistration is false, the parent prints msg on stderr
// and returns immediately. This is the nested-gmux path: an
// interactive user runs `gmux foo` from a shell already inside a
// gmux session and sees the message at their prompt; the session
// shows up in the UI when it registers.
func spawnDetached(args []string, msg string, waitForRegistration bool) {
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot find own binary: %v", err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		log.Fatalf("cannot open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	// args is the command-remainder after gmux flag parsing. Re-exec via
	// the internal `__run -- <cmd>` form (ADR 0009): the bare-command
	// shorthand no longer exists, and `--` delivers the command verbatim
	// even when its own args look like flags. Because the child's stdin
	// is /dev/null it runs non-interactively without trying to attach.
	cmd := exec.Command(self, reexecRunArgs(args)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull

	var handshakeRead, handshakeWrite *os.File
	if waitForRegistration {
		var err error
		handshakeRead, handshakeWrite, err = os.Pipe()
		if err != nil {
			log.Fatalf("failed to create handshake pipe: %v", err)
		}
		// Parent reads, child writes. cmd.ExtraFiles[0] becomes fd 3
		// in the child; GMUX_HANDSHAKE_FD tells the child which fd to
		// write to.
		cmd.ExtraFiles = []*os.File{handshakeWrite}
		cmd.Env = append(os.Environ(), handshakeFDEnv+"=3")
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start background session: %v", err)
	}
	cmd.Process.Release()

	if !waitForRegistration {
		if msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		return
	}

	// Close the parent's copy of the write end. The only writer is
	// now the child; if it dies without writing, our read returns
	// EOF with zero bytes — the unambiguous "child failed" signal.
	_ = handshakeWrite.Close()
	defer handshakeRead.Close()

	id, err := readHandshake(handshakeRead, handshakeTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start background session: %s\n", explainHandshakeFailure(err))
		os.Exit(1)
	}
	fmt.Println(id)
}

// explainHandshakeFailure converts a readHandshake error into a
// short human-readable reason for the stderr message a script
// developer sees when a detached run cannot return a session id.
func explainHandshakeFailure(err error) string {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Sprintf("registration timed out after %s", handshakeTimeout)
	case errors.Is(err, io.EOF):
		return "child process exited before registering"
	default:
		return err.Error()
	}
}

// ptyWriterFunc is an adapter to use a function as an io.Writer.
type ptyWriterFunc func([]byte) (int, error)

func (f ptyWriterFunc) Write(p []byte) (int, error) { return f(p) }
