package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// startStubGmuxd binds a Unix socket at the path registerWithGmuxd
// dials (StateDir()/gmuxd.sock) and answers POST /v1/register with the
// given status. Returns nothing; cleanup is registered on t.
func startStubGmuxd(t *testing.T, status int) {
	t.Helper()
	// paths.SocketPath() resolves under XDG_STATE_HOME/gmux.
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	sockPath := filepath.Join(stateHome, "gmux", "gmuxd.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
}

// TestRegisterWithGmuxdOutcomes pins the fatal/transient/ok
// classification that the orphan-reap fix depends on:
//
//   - 200 → registerOK
//   - 4xx (the invalid-session-id verdict) → registerFatal, so a
//     headless runner exits instead of lingering as an orphan
//   - 5xx / unreachable → registerUnavailable, so a runner keeps
//     serving while gmuxd is still coming up
func TestRegisterWithGmuxdOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   registerOutcome
	}{
		{"ok", http.StatusOK, registerOK},
		{"invalid_id_is_fatal", http.StatusBadRequest, registerFatal},
		{"server_error_is_transient", http.StatusBadGateway, registerUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startStubGmuxd(t, tc.status)
			got := registerWithGmuxd("sess-test", "/tmp/whatever.sock")
			if got != tc.want {
				t.Errorf("registerWithGmuxd outcome = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestReapOnFatalRegistration pins the orphan-reap decision: only a
// fatal (permanent) rejection of a headless runner tears the process
// down. Transient failures must never reap (gmuxd may still be coming
// up), and an interactive runner is spared even on a fatal verdict
// because its local terminal is still attached.
func TestReapOnFatalRegistration(t *testing.T) {
	cases := []struct {
		name        string
		outcome     registerOutcome
		interactive bool
		want        bool
	}{
		{"fatal_headless_reaps", registerFatal, false, true},
		{"fatal_interactive_spared", registerFatal, true, false},
		{"unavailable_headless_keeps_serving", registerUnavailable, false, false},
		{"unavailable_interactive_keeps_serving", registerUnavailable, true, false},
		{"ok_headless", registerOK, false, false},
		{"ok_interactive", registerOK, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reapOnFatalRegistration(tc.outcome, tc.interactive); got != tc.want {
				t.Errorf("reapOnFatalRegistration(%d, %v) = %v, want %v", tc.outcome, tc.interactive, got, tc.want)
			}
		})
	}
}
