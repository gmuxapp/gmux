package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux-run/internal/adapter"
	"github.com/gmuxapp/gmux/cli/gmux-run/internal/adapter/adapters"
	"github.com/gmuxapp/gmux/cli/gmux-run/internal/naming"
	"github.com/gmuxapp/gmux/cli/gmux-run/internal/ptyserver"
	"github.com/gmuxapp/gmux/cli/gmux-run/internal/session"
)

const version = "0.1.0"

func main() {
	log.SetPrefix("gmux-run: ")
	log.SetFlags(0)

	title := flag.String("title", "", "optional session title")
	cwd := flag.String("cwd", "", "working directory (default: current)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		args = []string{"pi"}
	}

	workDir := *cwd
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			log.Fatalf("cannot determine cwd: %v", err)
		}
	}

	sessionID := naming.SessionID()
	sockPath := filepath.Join("/tmp/gmux-sessions", sessionID+".sock")

	// Resolve adapter — specific adapters first, generic fallback last
	registry := adapter.NewRegistry()
	registry.Register(adapters.NewPi())
	registry.SetFallback(adapters.NewGeneric(0))
	a := registry.Resolve(args)

	// Let adapter prepare the command and env
	preparedCmd, adapterEnv := a.Prepare(adapter.PrepareContext{
		Command:    args,
		Cwd:        workDir,
		SessionID:  sessionID,
		SocketPath: sockPath,
	})

	// Build session title
	sessionTitle := *title
	if sessionTitle == "" {
		sessionTitle = strings.Join(args, " ")
	}

	// Create in-memory session state (replaces metadata files)
	state := session.New(session.Config{
		ID:         sessionID,
		Command:    args,
		Cwd:        workDir,
		Kind:       a.Name(),
		SocketPath: sockPath,
		Title:      sessionTitle,
	})

	// Common env vars — set for every child, per ADR-0005
	env := []string{
		"GMUX=1",
		"GMUX_SOCKET=" + sockPath,
		"GMUX_SESSION_ID=" + sessionID,
		"GMUX_ADAPTER=" + a.Name(),
		"GMUX_VERSION=" + version,
	}
	env = append(env, adapterEnv...)

	fmt.Printf("session:  %s\n", sessionID)
	fmt.Printf("adapter:  %s\n", a.Name())
	fmt.Printf("command:  %s\n", strings.Join(preparedCmd, " "))

	// Start PTY server
	srv, err := ptyserver.New(ptyserver.Config{
		Command:    preparedCmd,
		Cwd:        workDir,
		Env:        env,
		SocketPath: sockPath,
		Adapter:    a,
		State:      state,
	})
	if err != nil {
		log.Fatalf("failed to start: %v", err)
	}

	state.SetRunning(srv.Pid())
	fmt.Printf("pid:      %d\n", srv.Pid())
	fmt.Printf("socket:   %s\n", srv.SocketPath())
	fmt.Println("serving...")

	// Start silence checker for generic adapter
	if g, ok := a.(*adapters.Generic); ok {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-srv.Done():
					return
				case <-ticker.C:
					if status := g.CheckSilence(); status != nil {
						state.SetStatus(status)
					}
				}
			}
		}()
	}

	// Handle signals — clean shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-srv.Done():
		// Child exited
	case sig := <-sigCh:
		fmt.Printf("\nreceived %v, shutting down...\n", sig)
		srv.Shutdown()
	}

	exitCode := srv.ExitCode()
	state.SetExited(exitCode)
	fmt.Printf("exited:   %d\n", exitCode)
	os.Exit(exitCode)
}
