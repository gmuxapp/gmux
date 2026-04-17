package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gmuxapp/gmux/cli/gmux/internal/binhash"
	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
	"github.com/gmuxapp/gmux/cli/gmux/internal/naming"
	"github.com/gmuxapp/gmux/cli/gmux/internal/ptyserver"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/workspace"
)

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

	sessionID := naming.SessionID()
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

	if !interactive {
		fmt.Printf("session:  %s\n", sessionID)
		fmt.Printf("adapter:  %s\n", a.Name())
		fmt.Printf("command:  %s\n", strings.Join(args, " "))
	}

	// Start PTY server
	srv, err := ptyserver.New(ptyCfg)
	if err != nil {
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
	go registerWithGmuxd(sessionID, sockPath)

	if interactive {
		// Transparent mode: attach local terminal to the PTY
		attach, err := localterm.New(localterm.Config{
			PTYWriter: ptyWriterFunc(func(p []byte) (int, error) {
				return srv.WritePTY(p)
			}),
			ResizeFn: srv.Resize,
		})
		if err != nil {
			log.Fatalf("failed to attach terminal: %v", err)
		}
		srv.SetLocalOutput(attach)

		// In interactive mode:
		// - SIGHUP → detach local terminal, keep session running
		// - SIGINT/SIGTERM are consumed by raw mode and forwarded to child via PTY
		//   (but we still catch them on gmux in case raw mode is somehow bypassed)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-srv.Done():
			// Child exited — detach and exit
			attach.Detach()
		case <-attach.Done():
			// Local terminal gone (stdin closed) — session continues headless
			srv.SetLocalOutput(nil)
			// Wait for child to exit (session persists, accessible via web UI)
			<-srv.Done()
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				// Terminal closed — detach, keep session alive
				attach.Detach()
				srv.SetLocalOutput(nil)
				// Continue running headless until child exits
				<-srv.Done()
			} else {
				// SIGINT/SIGTERM — clean shutdown
				attach.Detach()
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
	os.Exit(exitCode)
}

// spawnDetached re-execs gmux with the given args as a setsid'd
// background process, disconnected from the current terminal. Used for
// both --no-attach and nested-gmux scenarios: the child registers with
// gmuxd and appears in the UI; the parent returns immediately.
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

	// args is the command-remainder after gmux flag parsing, so it does
	// not contain any gmux flags. The detached child sees no gmux flags,
	// takes the default run path, and — because its stdin is /dev/null —
	// runs non-interactively without trying to attach.
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start background session: %v", err)
	}
	cmd.Process.Release()
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// ptyWriterFunc is an adapter to use a function as an io.Writer.
type ptyWriterFunc func([]byte) (int, error)

func (f ptyWriterFunc) Write(p []byte) (int, error) { return f(p) }
