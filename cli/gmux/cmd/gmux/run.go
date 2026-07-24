package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
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

// detachedStartupBudget is the single end-to-end budget for explicit -d.
// Its absolute deadline is handed to the child, so process startup, daemon
// health/autostart, registration requests, backoff, and acknowledgement all
// consume the same clock.
const detachedStartupBudget = 30 * time.Second

// foregroundRegistrationBudget is the best-effort registration window for
// foreground and nested-gmux launches. These runners are not gate-blocked:
// the user's command is already running and the local terminal is attached.
// Registration is opportunistic — a short window avoids a visible hang at
// the shell when gmuxd is unreachable, while still giving a healthy daemon
// time to accept the POST. Explicit detach (-d) uses detachedStartupBudget
// because its registration context is shared with the user command's start
// gate and the parent waits for the full ack before printing the session id.
const foregroundRegistrationBudget = 3 * time.Second

// maxHandshakeFrameBytes caps each line read from the control pipe, preventing
// a wedged writer from growing parent memory indefinitely.
const maxHandshakeFrameBytes = 512

const detachedCleanupGrace = 3 * time.Second

const detachedTargetCleanupGrace = 2 * time.Second

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

// reapOnRegistrationFailure reports whether a runner should tear itself
// down because registration with gmuxd failed in a way it cannot recover
// from. Two distinct conditions qualify:
//
//   - outcome == registerFatal: gmuxd understood the request and refused it
//     permanently (4xx), e.g. its IsValidSessionID guard rejected the id.
//     Retrying or waiting changes nothing.
//   - handshakeOwned && outcome != registerOK: the runner is gate-blocking
//     an explicit -d launch; any non-success outcome (including the transient
//     registerUnavailable) means the parent will time out waiting for the
//     ack. Tearing down immediately and sending the failure ack is cleaner
//     than forcing the parent to sit the full 30 s deadline.
//
// In both cases, !interactive is required: the runner is headless (no local
// terminal attached), making gmuxd the only consumer. An interactive runner
// is spared even on a fatal verdict because its terminal is still usefully
// attached. The convIndex-rehydrate resume bug (a session keyed by its
// conversation UUID that could never register) is the canonical orphan example.
func reapOnRegistrationFailure(outcome registerOutcome, interactive, handshakeOwned bool) bool {
	return !interactive && (outcome == registerFatal || (handshakeOwned && outcome != registerOK))
}

// shutdownDetachedTarget closes the runner and explicitly supervises the PTY
// child's separate process group. The PTY library gives the target its own
// session, so killing only the runner's setsid group is insufficient when a
// target ignores terminal-close SIGHUP.
func shutdownDetachedTarget(srv *ptyserver.Server) {
	srv.Shutdown()
	pid := srv.Pid()
	terminateProcessGroup(pid, detachedTargetCleanupGrace)
	drainStructChan(srv.Done(), detachedTargetCleanupGrace)
}

// drainErrorChan waits for done within grace. If the timer fires first (e.g.
// a process stuck in uninterruptible kernel sleep that SIGKILL cannot wake),
// it returns and lets the goroutine feeding done reap the process
// asynchronously. This bounds cleanup time while preserving the single
// Wait-owner invariant.
func drainErrorChan(done <-chan error, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// drainStructChan is the same as drainErrorChan for <-chan struct{} (e.g.
// srv.Done()).
func drainStructChan(done <-chan struct{}, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func terminateProcessGroup(pgid int, grace time.Duration) {
	_ = syscall.Kill(-pgid, syscall.SIGHUP)
	deadline := time.Now().Add(grace)
	for processGroupExists(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupExists(pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// Bound the post-SIGKILL loop: a zombie-only group returns EPERM
		// from kill(2), which processGroupExists treats as "still exists".
		// After the deadline, treat any remainder as done — a zombie has
		// no further harm potential; a recycled group is not ours to track.
		killDeadline := time.Now().Add(grace)
		for processGroupExists(pgid) && time.Now().Before(killDeadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func processGroupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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

	registrationCtx, cancelRegistration, handshakeOwned := handshakeContext()
	defer cancelRegistration()
	if handshakeOwned && registrationCtx.Err() != nil {
		handshakeAck("", false)
		return
	}
	// Foreground and nested launches use a short best-effort budget rather
	// than the 30 s detach deadline: the user's command is already running
	// and the terminal is attached, so a 30 s hang on daemon-unavailable
	// would be visibly user-hostile. See foregroundRegistrationBudget.
	if !handshakeOwned {
		registrationCtx, cancelRegistration = context.WithTimeout(context.Background(), foregroundRegistrationBudget)
		defer cancelRegistration()
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

	// Common env vars — set for every child so nested gmux invocations can
	// discover their session context (see ADR 0003 for GMUX_SESSION_ID).
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
	var (
		controlFile    *os.File
		targetGateFile *os.File
	)
	if handshakeOwned {
		self, err := os.Executable()
		if err != nil {
			handshakeAck("", false)
			return
		}
		// Dup capturedHandshakeFD so the target wrapper's ExtraFiles entry owns
		// a separate fd; handshakeAck retains the original as the sole owner.
		// Without the dup, both ExtraFiles and handshakeAck would wrap the same
		// raw fd, creating two *os.File finalizers on one fd (the aliasing
		// footgun documented in handshake.go).
		controlDupFD, err := syscall.Dup(capturedHandshakeFD)
		if err != nil {
			handshakeAck("", false)
			return
		}
		// Set CLOEXEC on the dup for defence in depth; exec.Cmd ExtraFiles
		// re-dups with dup2 in the child regardless, so this does not prevent
		// the target wrapper from receiving fd 3.
		syscall.CloseOnExec(controlDupFD)
		controlFile = os.NewFile(uintptr(controlDupFD), "gmux-target-control")
		targetGateFile = os.NewFile(uintptr(capturedHandshakeGateFD), "gmux-target-gate")
		if controlFile == nil || targetGateFile == nil {
			handshakeAck("", false)
			return
		}
		ptyCfg.CommandWrapper = []string{self, "__detached-target"}
		ptyCfg.ExtraFiles = []*os.File{controlFile, targetGateFile}
		ptyCfg.Env = append(ptyCfg.Env, targetControlFDEnv+"=3", targetGateFDEnv+"=4")
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
	// Close the runner's ExtraFiles copies now that the target wrapper has
	// been forked. controlFile wraps the dup'd fd (not capturedHandshakeFD),
	// so closing it does not affect handshakeAck's use of the original.
	// capturedHandshakeGateFD is cleared so a subsequent captureHandshakeFD
	// call (e.g. from a nested runner) cannot misuse a stale value.
	if controlFile != nil {
		_ = controlFile.Close()
	}
	if targetGateFile != nil {
		_ = targetGateFile.Close() // only the parent and target retain the gate
		capturedHandshakeGateFD = -1
	}
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

	var detachedSigCh chan os.Signal
	if handshakeOwned && !interactive {
		detachedSigCh = make(chan os.Signal, 1)
		signal.Notify(detachedSigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(detachedSigCh)
	}

	// Auto-start gmuxd if not running (one-shot, never retried), then
	// register. The goroutine signals regDone when the registration
	// HTTP call has completed (succeeded or exhausted retries) and the
	// handshake — if any — has been delivered to the parent. We block
	// on regDone before exit so a fast-exiting command (echo, true,
	// false) can't lose the registration race.
	ensureGmuxdContext(registrationCtx)
	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		outcome := registerWithGmuxd(registrationCtx, sessionID, sockPath)
		if reapOnRegistrationFailure(outcome, interactive, handshakeOwned) {
			log.Printf("gmux: registration failed for %s; shutting down orphaned runner", sessionID)
			shutdownDetachedTarget(srv)
			handshakeAck(sessionID, false)
			return
		}
		handshakeAck(sessionID, outcome.ok())
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
		sigCh := detachedSigCh
		if sigCh == nil {
			sigCh = make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		}

		select {
		case <-srv.Done():
			// Child exited
		case sig := <-sigCh:
			fmt.Printf("\nreceived %v, shutting down...\n", sig)
			if handshakeOwned {
				shutdownDetachedTarget(srv)
			} else {
				srv.Shutdown()
			}
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
	// Bounded by the registration context (the shared explicit-detach
	// deadline, or the separate best-effort foreground/nested budget).
	<-regDone

	// Deregister from gmuxd (best-effort)
	deregisterFromGmuxd(sessionID)

	if !interactive {
		fmt.Printf("exited:   %d\n", exitCode)
	}
	waitForHandshakeRelease()
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

// runDetachedTarget publishes its separately-sessioned process-group ID before
// allowing the user command to exec. The parent owns the gate fd, so even if
// the runner dies in the fork/publication window, the command cannot start
// without the parent first learning which group it must supervise.
func runDetachedTarget(args []string) int {
	controlFD, err1 := strconv.Atoi(os.Getenv(targetControlFDEnv))
	gateFD, err2 := strconv.Atoi(os.Getenv(targetGateFDEnv))
	_ = os.Unsetenv(targetControlFDEnv)
	_ = os.Unsetenv(targetGateFDEnv)
	if err1 != nil || err2 != nil || controlFD < 3 || gateFD < 3 || len(args) == 0 {
		return 125
	}
	control := os.NewFile(uintptr(controlFD), "gmux-target-control")
	gate := os.NewFile(uintptr(gateFD), "gmux-target-gate")
	if control == nil || gate == nil {
		return 125
	}
	_, _ = fmt.Fprintf(control, "TARGET %d\n", os.Getpid())
	_ = control.Close()
	var token [1]byte
	_, err := io.ReadFull(gate, token[:])
	_ = gate.Close()
	if err != nil || token[0] != 'G' {
		return 125
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		return 127
	}
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		return 126
	}
	return 0
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

	var handshakeRead, handshakeWrite, gateRead, gateWrite, holdRead, holdWrite *os.File
	if waitForRegistration {
		var err error
		handshakeRead, handshakeWrite, err = os.Pipe()
		if err != nil {
			log.Fatalf("failed to create handshake pipe: %v", err)
		}
		gateRead, gateWrite, err = os.Pipe()
		if err != nil {
			log.Fatalf("failed to create target gate pipe: %v", err)
		}
		holdRead, holdWrite, err = os.Pipe()
		if err != nil {
			log.Fatalf("failed to create runner hold pipe: %v", err)
		}
		// Parent reads acknowledgements and owns the target-start gate.
		deadline := time.Now().Add(detachedStartupBudget)
		cmd.ExtraFiles = []*os.File{handshakeWrite, gateRead, holdRead}
		cmd.Env = append(os.Environ(),
			handshakeFDEnv+"=3",
			handshakeGateFDEnv+"=4",
			handshakeHoldFDEnv+"=5",
			handshakeDeadlineEnv+"="+fmt.Sprint(deadline.UnixNano()),
		)
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start background session: %v", err)
	}
	if !waitForRegistration {
		_ = cmd.Process.Release()
		if msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		return
	}

	// Close the parent's copy of the write end. The only writer is
	// now the child; if it dies without writing, our read returns
	// EOF with zero bytes — the unambiguous "child failed" signal.
	_ = handshakeWrite.Close()
	_ = gateRead.Close()
	_ = holdRead.Close()
	defer handshakeRead.Close()
	defer gateWrite.Close()
	defer holdWrite.Close()

	deadlineNanos, _ := strconv.ParseInt(envValue(cmd.Env, handshakeDeadlineEnv), 10, 64)
	id, err := awaitDetachedHandshake(cmd, handshakeRead, gateWrite, holdWrite, time.Unix(0, deadlineNanos), detachedCleanupGrace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start background session: %s\n", explainHandshakeFailure(err))
		os.Exit(1)
	}
	fmt.Println(id)
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

// awaitDetachedHandshake retains ownership until acknowledgement. Every
// failure kills the setsid process group and waits, escalating if graceful
// runner shutdown does not complete.
// readHandshakeFrame reads one newline-terminated frame from reader using
// ReadSlice, which returns at most reader's buffer capacity (maxHandshakeFrameBytes+1)
// of data before returning bufio.ErrBufferFull — so allocation is strictly
// bounded regardless of how much the child writes without a newline.
// ErrBufferFull is mapped to a descriptive error; other errors are passed through.
func readHandshakeFrame(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return "", fmt.Errorf("handshake frame too large (>%d bytes without newline)", maxHandshakeFrameBytes)
	}
	return strings.TrimSpace(string(line)), err
}

func awaitDetachedHandshake(cmd *exec.Cmd, r, gate, hold *os.File, deadline time.Time, grace time.Duration) (string, error) {
	// One goroutine owns Wait for every outcome. On success it remains until the
	// detached runner exits (or this short-lived CLI parent exits); on failure
	// cleanup joins it. This avoids both Release-on-zombie and double Wait races.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	targetPGID := 0
	id := ""
	var readErr error
	// Use a fixed-size buffer so ReadSlice can enforce the frame cap without
	// accumulating the full frame first. The +1 lets a frame of exactly
	// maxHandshakeFrameBytes bytes plus its newline fit before ErrBufferFull.
	reader := bufio.NewReaderSize(r, maxHandshakeFrameBytes+1)
	if err := r.SetReadDeadline(deadline); err != nil {
		// Route through the common cleanup path so cmd.Wait is always joined and
		// the target group is signalled — preserving the ownership invariant.
		readErr = err
	} else {
		for targetPGID == 0 || id == "" {
			line, err := readHandshakeFrame(reader)
			if err != nil {
				readErr = err
				break
			}
			if strings.HasPrefix(line, "TARGET ") {
				targetPGID, _ = strconv.Atoi(strings.TrimPrefix(line, "TARGET "))
				if targetPGID <= 0 {
					readErr = errors.New("invalid target process group")
					break
				}
			} else if line == "" {
				// Prompt error: silently skipping would wait out the full
				// deadline instead of surfacing the issue immediately.
				readErr = errors.New("empty session id from child")
				break
			} else {
				if !paths.IsValidSessionID(line) {
					readErr = fmt.Errorf("invalid session id %q", line)
					break
				}
				id = line
			}
		}
	}
	if readErr == nil {
		if _, err := gate.Write([]byte{'G'}); err != nil {
			readErr = err
		} else {
			_ = hold.Close()
			return id, nil
		}
	}

	// Signal before closing: while the gate is still open the target is
	// gate-blocked and its PGID is guaranteed valid, removing the reuse
	// window that opens when the gate closes first (wrapper exits → reaped
	// → PGID recycled before the signal fires). Closing the gate afterward
	// prevents exec even if the signal is delayed.
	if targetPGID > 0 {
		_ = syscall.Kill(-targetPGID, syscall.SIGHUP)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	_ = gate.Close()
	_ = hold.Close()
	timer := time.NewTimer(grace)
	select {
	case <-done:
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// Bounded: a D-state process may not become waitable after SIGKILL;
		// the Wait goroutine continues asynchronously once the kernel allows it.
		drainErrorChan(done, grace)
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if targetPGID == 0 {
		_ = r.SetReadDeadline(time.Now().Add(grace))
		for {
			// The same fixed-size reader caps the rescan too.
			line, err := readHandshakeFrame(reader)
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "TARGET ") {
				targetPGID, _ = strconv.Atoi(strings.TrimPrefix(line, "TARGET "))
				break
			}
		}
	}
	if targetPGID > 0 {
		terminateProcessGroup(targetPGID, grace)
	}
	return "", readErr
}

// explainHandshakeFailure converts a readHandshake error into a
// short human-readable reason for the stderr message a script
// developer sees when a detached run cannot return a session id.
func explainHandshakeFailure(err error) string {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Sprintf("registration timed out after %s", detachedStartupBudget)
	case errors.Is(err, io.EOF):
		return "child process exited before registering"
	default:
		return err.Error()
	}
}

// ptyWriterFunc is an adapter to use a function as an io.Writer.
type ptyWriterFunc func([]byte) (int, error)

func (f ptyWriterFunc) Write(p []byte) (int, error) { return f(p) }
