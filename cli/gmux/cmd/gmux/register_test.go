package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			got := registerWithGmuxd(ctx, "sess-test", "/tmp/whatever.sock")
			if got != tc.want {
				t.Errorf("registerWithGmuxd outcome = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestReapOnRegistrationFailure pins the reap decision for all three outcome ×
// interactive × handshakeOwned combinations:
//
//   - Fatal (4xx) + headless always reaps (permanent daemon rejection).
//   - Transient + headless + no handshake keeps serving (daemon may still start).
//   - Any non-OK + headless + handshakeOwned reaps (parent is gate-blocked;
//     waiting the full 30 s is worse than a prompt failure ack).
//   - Interactive runners are always spared (local terminal is attached).
func TestReapOnRegistrationFailure(t *testing.T) {
	cases := []struct {
		name           string
		outcome        registerOutcome
		interactive    bool
		handshakeOwned bool
		want           bool
	}{
		// Non-handshake paths (handshakeOwned=false)
		{"fatal_headless_reaps", registerFatal, false, false, true},
		{"fatal_interactive_spared", registerFatal, true, false, false},
		{"unavailable_headless_keeps_serving", registerUnavailable, false, false, false},
		{"unavailable_interactive_keeps_serving", registerUnavailable, true, false, false},
		{"ok_headless_keeps", registerOK, false, false, false},
		{"ok_interactive_keeps", registerOK, true, false, false},
		// Handshake-owned paths (handshakeOwned=true): any non-OK reaps the
		// headless runner so the parent's gate doesn't exhaust the full deadline.
		{"handshake_owned_unavailable_headless_reaps", registerUnavailable, false, true, true},
		{"handshake_owned_fatal_headless_reaps", registerFatal, false, true, true},
		{"handshake_owned_ok_headless_keeps", registerOK, false, true, false},
		{"handshake_owned_unavailable_interactive_spared", registerUnavailable, true, true, false},
		{"handshake_owned_fatal_interactive_spared", registerFatal, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reapOnRegistrationFailure(tc.outcome, tc.interactive, tc.handshakeOwned); got != tc.want {
				t.Errorf("reapOnRegistrationFailure(outcome=%v, interactive=%v, owned=%v) = %v, want %v",
					tc.outcome, tc.interactive, tc.handshakeOwned, got, tc.want)
			}
		})
	}
}
