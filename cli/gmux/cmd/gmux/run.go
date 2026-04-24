package main

import (
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
	"github.com/gmuxapp/gmux/packages/workspace"
)

// envForceSessionID carries a pre-generated session ID from a
// detaching parent (spawnDetached) to its re-execed child. This lets
// the parent announce the ID on stdout before the child exists yet, so
// `id=$(gmux --no-attach cmd)` works. It is intentionally undocumented
// for end users: the only legitimate setter is gmux itself.
//
// The child reads and immediately clears it (see nextSessionID) so a
// grandchild session launched later doesn't silently adopt the parent's
// id and collide in gmuxd's session store.
const envForceSessionID = "GMUX_RUNNER_FORCE_SESSION_ID"

// nextSessionID returns the session ID to use for this runner. When
// envForceSessionID holds a well-formed id (sess-<hex>), it is adopted
// and the env var is cleared. Otherwise a fresh id is generated. The
// env var is cleared even on a malformed value so a bad entry doesn't
// survive into child processes.
func nextSessionID() string {
	forced, ok := os.LookupEnv(envForceSessionID)
	if ok {
		os.Unsetenv(envForceSessionID)
	}
	if ok && isValidSessionID(forced) {
		return forced
	}
	return naming.SessionID()
}

// isValidSessionID checks the shape naming.SessionID produces:
// "sess-" followed by lowercase hex. Kept deliberately permissive on
// length (the generator currently uses 8 chars but this file shouldn't
// lock that in).
func isValidSessionID(id string) bool {
	const prefix = "sess-"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	rest := id[len(prefix):]
	if rest == "" {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// ptyDrainTimeout bounds the wait for the PTY to fully flush after the
// child exits. A well-behaved child has its PTY slave closed by the
// kernel as soon as it exits, so EOF on ptmx arrives almost immediately
// and this timeout is never approached in practice. The ceiling only
// matters when a grandchild is still holding the slave open: we'd
// rather restore the user's terminal promptly than wait forever on a
// background writer.
const ptyDrainTimeout = 250 * time.Millisecond

// runSession launches a new managed session for the given command.
//
// When attach is true and stdin is a tty, the local terminal is wired
// to the PTY so the command behaves transparently (the default). When
// attach is false, the session is spawned detached from the tty and
// this call returns immediately once the session is running, leaving
// the session visible in the gmux UI.
func runSession(args []string, attach bool) {
	// Nested gmux detection: if we're running interactively inside an
	// existing gmux session, re-exec as a detached headless process instead
	// of doing PTY passthrough (which would nest PTY-within-PTY). The
	// detached process registers with gmuxd and the session appears in the
	// gmux UI. The original process returns immediately to the parent shell.
	if os.Getenv("GMUX") == "1" && localterm.IsInteractive() {
		spawnDetached(args, "started "+strings.Join(args, " ")+" in background (visible in gmux)")
		return
	}

	// Explicit --no-attach: spawn detached and return immediately, whether
	// or not we're inside another gmux session. The session registers with
	// gmuxd on its own.
	if !attach {
		spawnDetached(args, "started "+strings.Join(args, " ")+" in background (visible in gmux)")
		return
	}

	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine cwd: %v", err)
	}

	sessionID := nextSessionID()
	socketDir := os.Getenv("GMUX_SOCKET_DIR")
	if socketDir == "" {
		socketDir = "/tmp/gmux-sessions"
	}
	sockPath := filepath.Join(socketDir, sessionID+".sock")

	// Resolve adapter — registered adapters first, shell fallback
	registry := adapter.NewRegistry()
	for _, a := range adapters.All {
		registry.Register(a)
	}
	registry.SetFallback(adapters.DefaultFallback())
	a := registry.Resolve(args)

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
		ID:            sessionID,
		Command:       args,
		Cwd:           workDir,
		Kind:          a.Name(),
		WorkspaceRoot: wsRoot,
		Remotes:       remotes,
		SocketPath:    sockPath,
		BinaryHash:    binhash.Self(),
		RunnerVersion: version,
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

	interactive := localterm.IsInteractive()

	// Determine initial PTY size — use terminal size if interactive
	ptyCfg := ptyserver.Config{
		Command:    args,
		Cwd:        workDir,
		Env:        env,
		SocketPath: sockPath,
		Adapter:    a,
		State:      state,
	}
	// Always try to inherit terminal dimensions from the parent.
	// Even non-interactive launches (background, piped) benefit from
	// a real size: the PTY and virtual terminal start correctly sized
	// instead of falling back to 80x24.
	if cols, rows, err := localterm.TerminalSize(); err == nil {
		ptyCfg.Cols = cols
		ptyCfg.Rows = rows
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

	// Start PTY server. Reuses the outer `err` declared by os.Getwd above.
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

	if !interactive {
		fmt.Printf("pid:      %d\n", srv.Pid())
		fmt.Printf("socket:   %s\n", srv.SocketPath())
		fmt.Println("serving...")
	}

	// Auto-start gmuxd if not running (one-shot, never retried), then register.
	ensureGmuxd()
	regDone := make(chan struct{})
	go func() {
		registerWithGmuxd(sessionID, sockPath)
		close(regDone)
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
	state.SetExited(exitCode)

	// Deregister from gmuxd (best-effort)
	deregisterFromGmuxd(sessionID)

	if !interactive {
		fmt.Printf("exited:   %d\n", exitCode)
	}

	// Wait for the scrollback tail file to finish writing before we exit.
	// waitChild runs persistTail *after* Done() fires (so Done() keeps its
	// "child exited" meaning for the interactive attach path), which means
	// os.Exit could otherwise cut the write off mid-flight and leave the
	// disk fallback for `gmux --tail` silently empty. The timeout bounds
	// any pathological I/O stall; the common-case wait is under a
	// millisecond.
	select {
	case <-srv.TailPersisted():
	case <-time.After(tailPersistTimeout):
	}

	// Wait for gmuxd registration to complete before we exit. For
	// ultra-fast commands (think `gmux echo hi`) the child can finish
	// before the register goroutine above has even posted to
	// /v1/register, leaving the session invisible to --list / --tail /
	// --send. Blocking on regDone here ensures the session record
	// survives the runner's lifetime.
	select {
	case <-regDone:
	case <-time.After(registerExitTimeout):
	}
	os.Exit(exitCode)
}

// registerExitTimeout bounds how long the runner waits for its own
// registration goroutine to finish before exiting. Sized to exceed
// registerWithGmuxd's full retry budget (5 attempts x 500 ms backoff),
// so we never abandon a registration that's still making progress.
const registerExitTimeout = 3 * time.Second

// tailPersistTimeout bounds how long runSession waits for the scrollback
// tail file to be written after the child exits. The write is a few KB
// to a local tmpfs in practice, so anything approaching this ceiling
// means disk I/O is pathologically slow and we'd rather exit than hang.
const tailPersistTimeout = 500 * time.Millisecond

// spawnDetached re-execs gmux with the given args as a setsid'd
// background process, disconnected from the current terminal. Used for
// both --no-attach and nested-gmux scenarios: the child registers with
// gmuxd and appears in the UI; the parent returns immediately.
//
// The session ID is generated here, in the parent, and passed to the
// child via envForceSessionID. This lets the parent print the short id
// to stdout before the child even execs, so callers can capture it:
//
//	id=$(gmux --no-attach pytest --watch)
//	gmux --tail 100 "$id"
//
// The human-readable message (e.g. "started ... in background") goes
// to stderr so stdout stays machine-parseable as "just the id".
func spawnDetached(args []string, msg string) {
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot find own binary: %v", err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		log.Fatalf("cannot open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	sessionID := naming.SessionID()

	// args is the command-remainder after gmux flag parsing, so it does
	// not contain any gmux flags. The detached child sees no gmux flags,
	// takes the default run path, and — because its stdin is /dev/null —
	// runs non-interactively without trying to attach.
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	// Pass the pre-generated id to the child. The child's nextSessionID
	// picks it up, clears it, and uses it instead of generating fresh.
	cmd.Env = append(os.Environ(), envForceSessionID+"="+sessionID)
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start background session: %v", err)
	}
	cmd.Process.Release()

	// Wait briefly for the child to register with gmuxd so a caller can
	// use the printed id immediately:
	//
	//	id=$(gmux --no-attach cmd); gmux --tail "$id"
	//
	// Without this, the caller races the child's async registration
	// flow (socket bind → /v1/register → gmuxd /meta query → store)
	// and gets "no session matches <id>" for fast-exiting commands.
	// On timeout we still announce: the id is valid, just not
	// ready-yet; the caller's next call will succeed once gmuxd
	// catches up.
	waitForRegistration(sessionID, registrationTimeout)
	announceDetached(os.Stdout, os.Stderr, sessionID, msg)
}

// registrationTimeout bounds the wait for a detached session to appear
// in gmuxd's session list. Sized for the common case (fork + bind +
// register ≈ tens of milliseconds) plus headroom for a cold-start
// gmuxd (≤ 1s on a warm disk).
const registrationTimeout = 2 * time.Second

// waitForRegistration polls gmuxd for sessionID until it appears or
// the timeout elapses. Returns true on success, false on timeout. The
// timeout path isn't fatal: the caller still announces the id, just
// without the ordering guarantee that an immediate follow-up command
// will find the session.
func waitForRegistration(sessionID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if sessions, err := fetchSessions(); err == nil {
			for _, s := range sessions {
				if s.ID == sessionID {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(registrationPollInterval)
	}
}

// registrationPollInterval is the gap between /v1/sessions polls while
// waiting for a just-spawned child to register. Small enough that
// typical registrations (tens of ms) aren't amplified by poll slack,
// large enough that we don't spin on gmuxd.
const registrationPollInterval = 25 * time.Millisecond

// announceDetached writes the new session's short id to stdout and the
// free-form human message (if any) to stderr. Split onto two streams so
// that `id=$(gmux --no-attach cmd)` captures exactly the id with no
// surrounding prose, while an interactive user still sees the
// "started ... in background" line in their terminal.
func announceDetached(stdout, stderr io.Writer, sessionID, msg string) {
	fmt.Fprintln(stdout, shortID(sessionID))
	if msg != "" {
		fmt.Fprintln(stderr, msg)
	}
}

// ptyWriterFunc is an adapter to use a function as an io.Writer.
type ptyWriterFunc func([]byte) (int, error)

func (f ptyWriterFunc) Write(p []byte) (int, error) { return f(p) }
