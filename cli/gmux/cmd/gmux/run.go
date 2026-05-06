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
		spawnDetached(args, "started "+strings.Join(args, " ")+" in background (visible in gmux)", false)
		return
	}

	// Explicit --no-attach: spawn detached, wait for the child to finish
	// registering with gmuxd, then print the session id on stdout so the
	// caller (typically a script) can capture it for --tail / --dismiss
	// without polling.
	if !attach {
		spawnDetached(args, "", true)
		return
	}

	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine cwd: %v", err)
	}

	// Honour GMUX_RESUME_ID when the daemon passed one in (resume,
	// restart). The runner uses the daemon-supplied id so the
	// session keeps its identity across the seam, including its
	// scrollback directory on disk. See ADR 0003.
	//
	// GMUX_RESUME_ID is intentionally distinct from GMUX_SESSION_ID:
	// the runner sets the latter for its child process (consumed by
	// adapters / hooks), so a nested `gmux foo` inside an
	// interactive gmux session inherits GMUX_SESSION_ID from the
	// parent and would otherwise try to re-bind the parent's id.
	// The dedicated GMUX_RESUME_ID is only ever set by the daemon
	// and not propagated to descendants, so the nested case falls
	// through to a fresh id without depending on the bind-collision
	// fallback as a safety net.
	sessionID := os.Getenv("GMUX_RESUME_ID")
	if sessionID == "" {
		sessionID = naming.SessionID()
	}
	socketDir := os.Getenv("GMUX_SOCKET_DIR")
	if socketDir == "" {
		socketDir = "/tmp/gmux-sessions"
	}
	sockPath := filepath.Join(socketDir, sessionID+".sock")

	// Bind the socket BEFORE any sessionID-dependent setup
	// (scrollback path, env, state). On collision with a live
	// runner — which typically only happens when a daemon-supplied
	// GMUX_RESUME_ID lands in a window where the targeted session
	// is actually still alive — fall back to a fresh id and bind
	// that instead. See ADR 0003 "Collision handling".
	os.MkdirAll(filepath.Dir(sockPath), 0o700)
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
		ok := registerWithGmuxd(sessionID, sockPath)
		handshakeAck(sessionID, ok)
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

// spawnDetached re-execs gmux with the given args as a setsid'd
// background process, disconnected from the current terminal. Used
// for both --no-attach and nested-gmux scenarios: the child registers
// with gmuxd and appears in the UI.
//
// When waitForRegistration is true, the parent blocks until the child
// either acknowledges registration via the handshake pipe or fails;
// on success it prints the session id on stdout, on failure it exits
// non-zero with a stderr error. This is the --no-attach path: scripts
// capture the id with id=$(gmux --no-attach foo) and use it
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

	// args is the command-remainder after gmux flag parsing, so it does
	// not contain any gmux flags. The detached child sees no gmux flags,
	// takes the default run path, and — because its stdin is /dev/null —
	// runs non-interactively without trying to attach.
	cmd := exec.Command(self, args...)
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
// developer sees when --no-attach can't return a session id.
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
