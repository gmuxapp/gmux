package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

type discoverTestAdapter struct {
	name      string
	available bool
}

func (a discoverTestAdapter) Name() string                      { return a.name }
func (a discoverTestAdapter) Discover() bool                    { return a.available }
func (a discoverTestAdapter) Match(_ []string) bool             { return false }
func (a discoverTestAdapter) Env(_ adapter.EnvContext) []string { return nil }
func (a discoverTestAdapter) Monitor(_ []byte) *adapter.Status  { return nil }
func (a discoverTestAdapter) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{ID: a.name, Label: a.name}}
}

func TestDiscoverAvailableAdaptersRunsAll(t *testing.T) {
	available := discoverAvailableAdapters([]adapter.Adapter{
		discoverTestAdapter{name: "pi", available: true},
		discoverTestAdapter{name: "opencode", available: false},
		discoverTestAdapter{name: "shell", available: true},
	})

	if !available["pi"] {
		t.Fatal("expected pi to be available")
	}
	if available["opencode"] {
		t.Fatal("expected opencode to be unavailable")
	}
	if !available["shell"] {
		t.Fatal("expected shell to be available")
	}
}

func TestLaunchersForAdaptersFiltersUnavailable(t *testing.T) {
	adapterList := []adapter.Adapter{
		discoverTestAdapter{name: "pi", available: true},
		discoverTestAdapter{name: "opencode", available: false},
		discoverTestAdapter{name: "shell", available: true},
	}

	launchers := launchersForAdapters(adapterList, map[string]bool{
		"pi":       true,
		"opencode": false,
		"shell":    true,
	})

	if len(launchers) != 2 {
		t.Fatalf("expected 2 available launchers, got %#v", launchers)
	}
	for _, l := range launchers {
		if !l.Available {
			t.Fatalf("expected launcher to be available: %#v", l)
		}
		if l.ID == "opencode" {
			t.Fatalf("did not expect unavailable launcher in config: %#v", l)
		}
	}
	if launchers[0].ID != "pi" || launchers[1].ID != "shell" {
		t.Fatalf("unexpected launcher order: %#v", launchers)
	}
}

func TestDiscoverLaunchersUsesCompiledAdapters(t *testing.T) {
	cfg := discoverLaunchers()
	if cfg.DefaultLauncher != "shell" {
		t.Fatalf("expected default launcher shell, got %q", cfg.DefaultLauncher)
	}
	if len(cfg.Launchers) < 1 {
		t.Fatalf("expected at least 1 launcher, got %d", len(cfg.Launchers))
	}

	seenShell := false
	for _, l := range cfg.Launchers {
		if !l.Available {
			t.Fatalf("did not expect unavailable launcher in config: %#v", l)
		}
		if l.ID == "shell" {
			seenShell = true
		}
	}
	if !seenShell {
		t.Fatalf("expected shell launcher in %#v", cfg.Launchers)
	}
	if got := cfg.Launchers[len(cfg.Launchers)-1].ID; got != "shell" {
		t.Fatalf("expected shell last, got %q", got)
	}
}

func TestRunWithoutArgsShowsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if got := stdout.String(); got == "" || !bytes.Contains(stdout.Bytes(), []byte("Usage: gmuxd")) {
		t.Fatalf("expected usage output, got %q", got)
	}
}

func TestRunHelpCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if got := stdout.String(); got == "" || !bytes.Contains(stdout.Bytes(), []byte("Usage: gmuxd")) {
		t.Fatalf("expected usage output, got %q", got)
	}
}

func TestRunStartHelpCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"start", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if got := stdout.String(); !bytes.Contains([]byte(got), []byte("Usage: gmuxd start [--replace]")) {
		t.Fatalf("expected start usage output, got %q", got)
	}
}

func TestRunVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if got := stdout.String(); !bytes.Contains([]byte(got), []byte(version)) {
		t.Fatalf("expected version output, got %q", got)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !bytes.Contains([]byte(got), []byte("unknown command")) || !bytes.Contains([]byte(got), []byte("Usage: gmuxd")) {
		t.Fatalf("expected error and usage output, got %q", got)
	}
}

func TestRunStartRejectsUnknownOption(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"start", "--wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !bytes.Contains([]byte(got), []byte("unknown option")) {
		t.Fatalf("expected unknown option error, got %q", got)
	}
}

func TestPrepareDaemonAddrReturnsOKWhenDaemonNotRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	var stderr bytes.Buffer
	if code := prepareDaemonAddr(addr, false, &stderr); code != 0 {
		t.Fatalf("expected code 0, got %d with stderr %q", code, stderr.String())
	}
}

func TestPrepareDaemonAddrFailsWhenDaemonAlreadyRunning(t *testing.T) {
	addr, srv, _ := startTestDaemon(t)
	defer srv.Close()

	var stderr bytes.Buffer
	if code := prepareDaemonAddr(addr, false, &stderr); code != 1 {
		t.Fatalf("expected code 1, got %d", code)
	}
	if got := stderr.String(); !bytes.Contains([]byte(got), []byte("already running")) {
		t.Fatalf("expected already running error, got %q", got)
	}
}

func TestRequestShutdownReturnsFalseWhenDaemonNotRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if requestShutdown(addr) {
		t.Fatalf("expected no shutdown request to succeed on %s", addr)
	}
}

func TestRequestShutdownStopsRunningDaemon(t *testing.T) {
	addr, srv, shutdownDone := startTestDaemon(t)
	defer srv.Close()

	if !requestShutdown(addr) {
		t.Fatalf("expected shutdown request to succeed for %s", addr)
	}

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestPrepareDaemonAddrReplacesRunningDaemon(t *testing.T) {
	addr, srv, shutdownDone := startTestDaemon(t)
	defer srv.Close()

	var stderr bytes.Buffer
	if code := prepareDaemonAddr(addr, true, &stderr); code != 0 {
		t.Fatalf("expected code 0, got %d with stderr %q", code, stderr.String())
	}

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestRunAuthLinkNoNetworkListener(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GMUXD_LISTEN", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{"auth-link"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("not configured")) {
		t.Fatalf("expected 'not configured' message, got %q", stderr.String())
	}
}

func TestRunAuthLinkWithNetworkListener(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("GMUXD_LISTEN", "10.0.0.5")

	var stdout, stderr bytes.Buffer
	code := run([]string{"auth-link"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "10.0.0.5:8790") {
		t.Errorf("expected address in output, got %q", out)
	}
	if !strings.Contains(out, "/auth/login?token=") {
		t.Errorf("expected auth URL in output, got %q", out)
	}
}

func startTestDaemon(t *testing.T) (string, *http.Server, <-chan struct{}) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	shutdownDone := make(chan struct{})

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			defer close(shutdownDone)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	})

	go func() {
		_ = srv.Serve(ln)
	}()

	return addr, srv, shutdownDone
}
