package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statusWithHealth starts a fake daemon on the standard socket path that
// answers /v1/health with the supplied data, points XDG_STATE_HOME at it, and
// runs `gmuxd status`. Returns stdout.
func statusWithHealth(t *testing.T, data map[string]any) string {
	t.Helper()
	stateDir := t.TempDir()
	sockDir := filepath.Join(stateDir, "gmux")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "gmuxd.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sockPath) })
	t.Setenv("XDG_STATE_HOME", stateDir)
	time.Sleep(50 * time.Millisecond)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, stderr.String())
	}
	return stdout.String()
}

func baseHealth() map[string]any {
	return map[string]any{
		"service": "gmuxd", "version": "1.0.0", "status": "ready",
		"listen": "127.0.0.1:8790",
	}
}

// TestRunStatusUpstreamAccessAlertNegativeSpace is the table test for the
// alert's NEGATIVE space. The PR asserted only the positive (key_expired
// alerts), so alerting on every state (`default:` in the switch) survived the
// whole suite: the operator's terminal would shout "run tailscale up" for a
// healthy node, a container, a Windows box, and a wedged probe alike.
//
// The alert (⚠ + remediation) is reserved for the two POSITIVE
// classifications. Everything else prints no warning and no `tailscale up`.
func TestRunStatusUpstreamAccessAlertNegativeSpace(t *testing.T) {
	tests := []struct {
		state     string
		detail    string
		hint      string
		wantAlert bool
		why       string
	}{
		{"key_expired", "local tailscale node key EXPIRED — dials to peers cannot succeed", "run `tailscale up` to re-authenticate", true, "positive: the key is expired"},
		{"tailscale_not_running", "tailscale backend is NeedsLogin — peers are unreachable", "log in / start tailscale: `tailscale up`", true, "positive: tailscaled answered with a logged-out state"},
		{"ok", "tailscale key valid until 2027-02-16T05:18:54Z", "", false, "healthy: no outage to announce"},
		{"probe_failed", "unavailable (probe failed: probe timed out after 3s)", "", false, "inconclusive: our probe, not their outage"},
		{"not_installed", "tailscale CLI not found (plain-LAN peers do not need it)", "", false, "not applicable, not an outage"},
		{"unsupported", "tailscale status is not probed on this platform", "", false, "declined, not an outage"},
		{"unknown", "probe in progress", "", false, "no evidence yet"},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			d := baseHealth()
			d["upstream_access"] = map[string]any{
				"state": tt.state, "detail": tt.detail,
			}
			if tt.hint != "" {
				d["upstream_access"].(map[string]any)["hint"] = tt.hint
			}
			out := statusWithHealth(t, d)

			hasAlert := strings.Contains(out, "\u26a0")
			if hasAlert != tt.wantAlert {
				t.Errorf("alert glyph present = %v, want %v (%s)\n%s", hasAlert, tt.wantAlert, tt.why, out)
			}
			if !tt.wantAlert {
				// The remediation must not leak through any path: the whole
				// point of F2 is that the daemon never advises on evidence it
				// does not have.
				for _, forbidden := range []string{"tailscale up", "re-authenticate", "start tailscaled"} {
					if strings.Contains(out, forbidden) {
						t.Errorf("state %q rendered advice %q (%s)\n%s", tt.state, forbidden, tt.why, out)
					}
				}
			}
		})
	}
}

// TestRunStatusProbeFailedIsInconclusiveNotOperatorAdvice is F2's operator-
// visible half: the daemon's own SIGKILL of a wedged `tailscale` used to print
// "⚠ upstream access: tailscale status unavailable: signal: killed / start
// tailscaled and log in: `tailscale up`" — advice to re-authenticate a node
// that may be perfectly healthy, cached and re-served for a full TTL.
func TestRunStatusProbeFailedIsInconclusiveNotOperatorAdvice(t *testing.T) {
	d := baseHealth()
	d["upstream_access"] = map[string]any{
		"state":  "probe_failed",
		"detail": "unavailable (probe failed: probe timed out after 3s (signal: killed))",
	}
	out := statusWithHealth(t, d)

	if !strings.Contains(out, "upstream access: unavailable (probe failed:") {
		t.Errorf("expected an inconclusive diagnostic line, got:\n%s", out)
	}
	if strings.Contains(out, "\u26a0") {
		t.Errorf("an inconclusive probe must not raise an alert:\n%s", out)
	}
	if strings.Contains(out, "tailscale up") {
		t.Errorf("an inconclusive probe must not carry remediation advice:\n%s", out)
	}
}

// TestRunStatusKeyExpiryTimestampIsShown is F10: the daemon shipped key_expiry
// and never printed it. An operator triaging an outage at 3am needs to know
// when the key died.
func TestRunStatusKeyExpiryTimestampIsShown(t *testing.T) {
	d := baseHealth()
	d["upstream_access"] = map[string]any{
		"state":      "key_expired",
		"detail":     "local tailscale node key EXPIRED (2026-05-05T10:00:00Z) — dials to peers cannot succeed",
		"hint":       "run `tailscale up` to re-authenticate (gmux only reports this)",
		"key_expiry": "2026-05-05T10:00:00Z",
	}
	out := statusWithHealth(t, d)
	if !strings.Contains(out, "2026-05-05T10:00:00Z") {
		t.Errorf("expected the key expiry timestamp in the alert, got:\n%s", out)
	}
}

// TestRunStatusFirstDialIsNotZeroAttempts is F4: a peer mid-first-dial is
// never_connected with zero failures, which rendered as
// "never connected (0 attempts)" — a failure with a count of nothing.
func TestRunStatusFirstDialIsNotZeroAttempts(t *testing.T) {
	d := baseHealth()
	d["peers"] = []map[string]any{
		{"name": "firstdial", "url": "https://100.96.12.34:8790", "status": "connecting", "never_connected": true, "consecutive_failures": 0},
	}
	out := statusWithHealth(t, d)
	if strings.Contains(out, "0 attempts") {
		t.Errorf("first dial must not render as \"0 attempts\":\n%s", out)
	}
	if !strings.Contains(out, "first attempt") {
		t.Errorf("expected a first-attempt rendering, got:\n%s", out)
	}
}

// TestRunStatusNeverConnectedShowsDaemonStart is F5: the counters are per-boot
// and gmuxd restarts automatically on version changes, so a peer that connected
// for weeks reads "never connected" after an upgrade. The boot time next to the
// count makes the per-boot semantics visible.
func TestRunStatusNeverConnectedShowsDaemonStart(t *testing.T) {
	started := time.Date(2026, 10, 14, 7, 3, 0, 0, time.UTC)
	d := baseHealth()
	d["started_at"] = started.Format(time.RFC3339)
	// The marker is rendered in the operator's local time, same as the CLI's
	// other timestamps.
	wantHHMM := started.Local().Format("15:04")
	d["peers"] = []map[string]any{
		{"name": "spoke", "url": "https://100.96.12.34:8790", "status": "disconnected",
			"last_error": "connection timed out", "consecutive_failures": 9, "never_connected": true},
	}
	out := statusWithHealth(t, d)
	if !strings.Contains(out, "never connected (9 failed attempts, since ") {
		t.Errorf("expected the never-connected count with a daemon-start time, got:\n%s", out)
	}
	if !strings.Contains(out, wantHHMM) {
		t.Errorf("expected the daemon start time (%s) in the render, got:\n%s", wantHHMM, out)
	}
}

// TestRunStatusOmitsDaemonStartForOlderDaemons: started_at is additive. An
// older daemon omits it, and the render must degrade to the pre-PR shape
// instead of printing a zero time.
func TestRunStatusOmitsDaemonStartForOlderDaemons(t *testing.T) {
	d := baseHealth()
	d["peers"] = []map[string]any{
		{"name": "spoke", "url": "https://100.96.12.34:8790", "status": "disconnected",
			"last_error": "connection refused", "consecutive_failures": 4, "never_connected": true},
	}
	out := statusWithHealth(t, d)
	if strings.Contains(out, "since") {
		t.Errorf("a daemon with no started_at must not render a start time, got:\n%s", out)
	}
	if !strings.Contains(out, "never connected (4 failed attempts): connection refused") {
		t.Errorf("expected the pre-PR render for an older daemon, got:\n%s", out)
	}
}

// TestRunStatusSanitizesExternallySourcedText is F9: every field on this
// output is externally sourced — the peer name/url/last_error ride the health
// wire from a (possibly hostile) spoke, and detail is the first line of the
// `tailscale` binary. Without sanitising, a compromised tailscale CLI or a
// malicious peer can set the window title, paint colours, move the cursor, or
// forge extra status lines.
func TestRunStatusSanitizesExternallySourcedText(t *testing.T) {
	d := baseHealth()
	d["upstream_access"] = map[string]any{
		"state":  "key_expired",
		"detail": "EXPIRED\x1b]0;pwned\x07\x1b[31m red\x1b[0m",
		"hint":   "harmless",
	}
	d["peers"] = []map[string]any{
		{
			"name": "evil\x1b[1;31m", "url": "https://x\x07.example", "status": "disconnected",
			// A newline in a wire field must not forge a whole extra line;
			// "\n  \u26a0 forged alert" inside last_error is the attack shape.
			"last_error": "boom\n  \u26a0 forged alert\x1b[5m", "consecutive_failures": 2, "never_connected": true,
		},
	}
	out := statusWithHealth(t, d)

	for name, seq := range map[string]string{
		"ESC":              "\x1b",
		"BEL":              "\x07",
		"newline-in-field": "\n  \u26a0 forged alert",
	} {
		if strings.Contains(out, seq) {
			t.Errorf("%s survived into daemon status output:\n%q", name, out)
		}
	}
	// The text is still legible: sanitising strips control bytes, not content.
	for _, want := range []string{"EXPIRED", "red", "boom", "forged alert", "evil", "x.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q to survive sanitising, got:\n%s", want, out)
		}
	}
	// The alert is the daemon's decision, not the peer's: the peer's payload
	// may contain an alert glyph as inert text on its own line, but it must not
	// be able to START a line (that is the forged-alert attack, via \n in a
	// wire field).
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "\u26a0") && !strings.Contains(line, "upstream access") {
			t.Errorf("a wire-sourced field forged an alert line: %q\nfull output:\n%s", line, out)
		}
	}
	if n := strings.Count(out, "upstream access"); n != 1 {
		t.Errorf("found %d upstream-access lines, want exactly 1:\n%s", n, out)
	}
}

func TestSanitizeTerminal(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "hello", "hello"},
		{"utf8 preserved", "key EXPIRED — dials", "key EXPIRED — dials"},
		{"csi colour", "a\x1b[31mred\x1b[0m", "ared"},
		{"osc title with BEL", "a\x1b]0;title\x07b", "ab"},
		{"osc title with ST", "a\x1b]0;title\x1b\\b", "ab"},
		{"dcs with ST", "a\x1bPq#5b\x1b\\c", "ac"},
		{"cursor move", "a\x1b[2;3Hb", "ab"},
		{"erase display", "a\x1b[2Jb", "ab"},
		{"newline flattened", "a\nb", "a b"},
		{"carriage return", "a\rb", "a b"},
		{"tab", "a\tb", "a b"},
		{"nul byte", "a\x00b", "ab"},
		{"del", "a\x7fb", "ab"},
		{"c1 as utf8", "a\u009bb", "ab"},
		{"truncated osc drops the tail", "a\x1b]0;pwn", "a"},
		{"lone esc", "a\x1b", "a"},
		{"fe escape", "a\x1bMc", "ac"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeTerminal(tt.in); got != tt.want {
				t.Errorf("sanitizeTerminal(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSinceDaemon proves the per-boot marker parses the published start time
// and degrades silently when it is missing or malformed (older daemon, future
// format change): the count must stay readable.
func TestSinceDaemon(t *testing.T) {
	if got := sinceDaemon(""); got != "" {
		t.Errorf("sinceDaemon(empty) = %q, want empty", got)
	}
	if got := sinceDaemon("not-a-time"); got != "" {
		t.Errorf("sinceDaemon(garbage) = %q, want empty", got)
	}
	got := sinceDaemon("2026-10-14T07:03:00Z")
	if !strings.HasPrefix(got, ", since ") || !strings.Contains(got, ":") {
		t.Errorf("sinceDaemon = %q, want \", since <hh:mm>\"", got)
	}
}
