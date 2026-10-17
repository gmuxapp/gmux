package tsauth

import (
	"strings"
	"testing"
	"time"
)

// The status line is the whole product surface for tailscale self-reach:
// every state must map to exactly one single-line sentence, and the state
// strings are health wire contract.
func TestSelfReachFromDiag_StateToLine(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-48 * time.Hour).Format(time.RFC3339)
	future := now.Add(30 * 24 * time.Hour).Format(time.RFC3339)

	tests := []struct {
		name        string
		enabled     bool
		diag        DiagStatus
		wantState   string
		wantLine    string
		wantURL     string
		wantAuthURL string
	}{
		{
			name:      "serving",
			enabled:   true,
			diag:      DiagStatus{FQDN: "gmux-lab.tailnet.ts.net", HTTPS: true, MagicDNS: true, BackendState: "Running", Connected: true},
			wantState: ReachReachable,
			wantLine:  "reachable at https://gmux-lab.tailnet.ts.net",
			wantURL:   "https://gmux-lab.tailnet.ts.net",
		},
		{
			name:        "node key expired with a login URL",
			enabled:     true,
			diag:        DiagStatus{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc123", Expired: true, KeyExpiry: expired},
			wantState:   ReachKeyExpired,
			wantLine:    "NOT reachable — node key expired 2026-10-15 12:00 UTC; run 'gmux remote' and open the login URL it prints: https://login.tailscale.com/a/abc123",
			wantAuthURL: "https://login.tailscale.com/a/abc123",
		},
		{
			name:      "node key expired while the backend still says Running",
			enabled:   true,
			diag:      DiagStatus{FQDN: "gmux-lab.tailnet.ts.net", HTTPS: true, BackendState: "Running", Connected: true, Expired: true, KeyExpiry: expired},
			wantState: ReachKeyExpired,
			wantLine:  "NOT reachable — node key expired 2026-10-15 12:00 UTC; run 'gmux remote' and open the login URL it prints",
		},
		{
			name:      "expiry already passed before tailscale flags it",
			enabled:   true,
			diag:      DiagStatus{BackendState: "NeedsLogin", KeyExpiry: expired},
			wantState: ReachKeyExpired,
			wantLine:  "NOT reachable — node key expired 2026-10-15 12:00 UTC; run 'gmux remote' and open the login URL it prints",
		},
		{
			name:      "expired flag without a usable timestamp",
			enabled:   true,
			diag:      DiagStatus{BackendState: "NeedsLogin", Expired: true, KeyExpiry: "not-a-time"},
			wantState: ReachKeyExpired,
			wantLine:  "NOT reachable — node key expired; run 'gmux remote' and open the login URL it prints",
		},
		{
			name:      "a future expiry is not a failure",
			enabled:   true,
			diag:      DiagStatus{FQDN: "gmux-lab.tailnet.ts.net", HTTPS: true, BackendState: "Running", Connected: true, KeyExpiry: future},
			wantState: ReachReachable,
			wantLine:  "reachable at https://gmux-lab.tailnet.ts.net",
			wantURL:   "https://gmux-lab.tailnet.ts.net",
		},
		{
			name:        "needs login (first run)",
			enabled:     true,
			diag:        DiagStatus{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/first"},
			wantState:   ReachNeedsLogin,
			wantLine:    "NOT reachable — the node needs login (state NeedsLogin); run 'gmux remote' and open the login URL it prints: https://login.tailscale.com/a/first",
			wantAuthURL: "https://login.tailscale.com/a/first",
		},
		{
			name:      "no state at all",
			enabled:   true,
			diag:      DiagStatus{BackendState: "NoState"},
			wantState: ReachNeedsLogin,
			wantLine:  "NOT reachable — the node needs login (state NoState); run 'gmux remote' and open the login URL it prints",
		},
		{
			name:      "starting",
			enabled:   true,
			diag:      DiagStatus{BackendState: "Starting"},
			wantState: ReachStarting,
			wantLine:  "starting — the tailscale node is not up yet (state Starting)",
		},
		{
			name:      "state not known yet",
			enabled:   true,
			diag:      DiagStatus{},
			wantState: ReachStarting,
			wantLine:  "starting — the tailscale node is not up yet",
		},
		{
			name:      "running but the name is not resolved yet",
			enabled:   true,
			diag:      DiagStatus{BackendState: "Running", HTTPS: true},
			wantState: ReachStarting,
			wantLine:  "starting — the tailnet name is not known yet",
		},
		{
			name:      "running without HTTPS",
			enabled:   true,
			diag:      DiagStatus{FQDN: "gmux-lab.tailnet.ts.net", BackendState: "Running"},
			wantState: ReachNoHTTPS,
			wantLine:  "NOT reachable — HTTPS is not enabled in this tailnet (enable it at https://login.tailscale.com/admin/dns)",
		},
		{
			// ListenTLS fails without certificates, so the listener never gets
			// a name: the missing name is the certificate problem's symptom,
			// and must not be reported as a node that is still coming up.
			name:      "running without HTTPS, name never resolved",
			enabled:   true,
			diag:      DiagStatus{BackendState: "Running"},
			wantState: ReachNoHTTPS,
			wantLine:  "NOT reachable — HTTPS is not enabled in this tailnet (enable it at https://login.tailscale.com/admin/dns)",
		},
		{
			name:      "stopped backend",
			enabled:   true,
			diag:      DiagStatus{BackendState: "Stopped"},
			wantState: ReachStopped,
			wantLine:  "NOT reachable — the tailscale node is stopped; run 'gmux remote' and open the login URL it prints",
		},
		{
			// A failed read is unknown, not healthy: a stale FQDN must not
			// keep reporting "reachable" once the node stopped answering.
			name:      "the node did not answer",
			enabled:   true,
			diag:      DiagStatus{FQDN: "gmux-lab.tailnet.ts.net", HTTPS: true, BackendState: "Running", Connected: true, StatusError: "context deadline exceeded"},
			wantState: ReachUnavailable,
			wantLine:  "NOT reachable — the tailscale node did not answer (context deadline exceeded)",
		},
		{
			name:      "remote access is off",
			enabled:   false,
			wantState: ReachDisabled,
			wantLine:  "disabled — remote access is off (run 'gmux remote' to enable)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SelfReachFromDiag(tt.diag, tt.enabled, now)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
			if got.Line != tt.wantLine {
				t.Errorf("Line =\n  %q\nwant\n  %q", got.Line, tt.wantLine)
			}
			if strings.Contains(got.Line, "\n") {
				t.Errorf("Line must be a single line, got %q", got.Line)
			}
			if got.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tt.wantURL)
			}
			if got.AuthURL != tt.wantAuthURL {
				t.Errorf("AuthURL = %q, want %q", got.AuthURL, tt.wantAuthURL)
			}
			if !strings.HasPrefix(got.Line, "reachable at ") && !strings.HasPrefix(got.Line, "NOT reachable — ") &&
				!strings.HasPrefix(got.Line, "starting — ") && !strings.HasPrefix(got.Line, "disabled — ") {
				t.Errorf("Line %q must start with a known prefix", got.Line)
			}
			// gmuxd's node is a tsnet node: the system tailscale CLI must
			// never be the suggested fix.
			if strings.Contains(got.Line, "tailscale up") {
				t.Errorf("Line %q must not advise the system 'tailscale up'", got.Line)
			}
		})
	}
}

// The key-expiry timestamp is reported verbatim so a health consumer can act
// on it, independent of the human-readable line.
func TestSelfReachFromDiag_CarriesKeyExpiry(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Hour).Format(time.RFC3339)
	got := SelfReachFromDiag(DiagStatus{BackendState: "NeedsLogin", Expired: true, KeyExpiry: expired}, true, now)
	if got.KeyExpiry != expired {
		t.Errorf("KeyExpiry = %q, want %q", got.KeyExpiry, expired)
	}
}

// A listener whose tsnet node never came up has no LocalClient: Diag reports
// no backend state, and the mapping must call that "starting", not a failure.
func TestSelfReachFromDiag_NoLocalClientIsStarting(t *testing.T) {
	got := SelfReachFromDiag(DiagStatus{FQDN: ""}, true, time.Now())
	if got.State != ReachStarting {
		t.Errorf("State = %q, want %q", got.State, ReachStarting)
	}
}

// The unix health server starts before startup convergence creates the tsnet
// listener, so a nil listener must never claim remote access is off when the
// config turns it on: that line tells the user to enable what they enabled.
func TestSelfReachNoListener(t *testing.T) {
	enabled := SelfReachNoListener(true)
	if enabled.State != ReachStarting {
		t.Errorf("enabled: State = %q, want %q", enabled.State, ReachStarting)
	}
	if enabled.Line != "starting — the tailscale node is not up yet" {
		t.Errorf("enabled: Line = %q", enabled.Line)
	}
	if strings.Contains(enabled.Line, "disabled") || strings.Contains(enabled.Line, "to enable") {
		t.Errorf("enabled: Line %q must not tell the user to enable tailscale", enabled.Line)
	}

	off := SelfReachNoListener(false)
	if off.State != ReachDisabled {
		t.Errorf("disabled: State = %q, want %q", off.State, ReachDisabled)
	}
	if off.Line != "disabled — remote access is off (run 'gmux remote' to enable)" {
		t.Errorf("disabled: Line = %q", off.Line)
	}
}
