package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter/adapters"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/naming"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/ptyserver"
	"github.com/gmuxapp/gmux/cli/gmuxr/internal/session"
)

const version = "0.1.0"

func main() {
	log.SetPrefix("gmuxr: ")
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
	socketDir := os.Getenv("GMUX_SOCKET_DIR")
	if socketDir == "" {
		socketDir = "/tmp/gmux-sessions"
	}
	sockPath := filepath.Join(socketDir, sessionID+".sock")

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

	// Register with gmuxd (best-effort, non-blocking)
	go registerWithGmuxd(sessionID, sockPath)

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

	// Deregister from gmuxd (best-effort)
	deregisterFromGmuxd(sessionID)

	fmt.Printf("exited:   %d\n", exitCode)
	os.Exit(exitCode)
}

func registerWithGmuxd(sessionID, socketPath string) {
	gmuxdAddr := os.Getenv("GMUXD_ADDR")
	if gmuxdAddr == "" {
		gmuxdAddr = "http://localhost:8790"
	}

	payload, _ := json.Marshal(map[string]string{
		"session_id":  sessionID,
		"socket_path": socketPath,
	})

	// Retry a few times — gmuxr may start before the HTTP server is ready
	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Post(gmuxdAddr+"/v1/register", "application/json", bytes.NewReader(payload))
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			log.Printf("registered with gmuxd")
			return
		}
	}
	log.Printf("could not register with gmuxd (will be discovered via socket scan)")
}

func deregisterFromGmuxd(sessionID string) {
	gmuxdAddr := os.Getenv("GMUXD_ADDR")
	if gmuxdAddr == "" {
		gmuxdAddr = "http://localhost:8790"
	}

	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(gmuxdAddr+"/v1/deregister", "application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	resp.Body.Close()
}
