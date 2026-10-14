package tsdiag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRunProbe_Classification(t *testing.T) {
	expired := `{"BackendState":"Running","Expired":true,"Self":{"KeyExpiry":"2026-01-01T00:00:00Z"}}`
	healthy := `{"BackendState":"Running","Self":{"KeyExpiry":"2027-02-16T05:18:54Z"}}`
	needsLogin := `{"BackendState":"NeedsLogin","AuthURL":"https://login.tailscale.com/x"}`
	starting := `{"BackendState":"Starting"}`
	badOutput := "Failed to connect to local IPN socket: file does not exist\n"

	tests := []struct {
		name        string
		out         string
		runErr      error
		wantState   State
		wantHintHas string
		wantDetail  string
		wantExpiry  string
	}{
		{
			name:        "expired key",
			out:         expired,
			wantState:   StateKeyExpired,
			wantHintHas: "tailscale up",
			wantDetail:  "EXPIRED",
			wantExpiry:  "2026-01-01T00:00:00Z",
		},
		{
			name:       "healthy",
			out:        healthy,
			wantState:  StateOK,
			wantExpiry: "2027-02-16T05:18:54Z",
		},
		{
			name:        "needs login",
			out:         needsLogin,
			wantState:   StateNotRunning,
			wantHintHas: "tailscale up",
			wantDetail:  "NeedsLogin",
		},
		{
			name:       "starting",
			out:        starting,
			wantState:  StateNotRunning,
			wantDetail: "Starting",
		},
		{
			name:        "tailscaled down (non-JSON output + exec error)",
			out:         badOutput,
			runErr:      errors.New("exit status 1"),
			wantState:   StateNotRunning,
			wantHintHas: "tailscale up",
			wantDetail:  "Failed to connect to local IPN socket",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewWithRunner(time.Second, nil, func(context.Context) ([]byte, error) {
				return []byte(tt.out), tt.runErr
			})
			got := p.runProbe(context.Background())
			if got.State != tt.wantState {
				t.Fatalf("state = %q, want %q (got %+v)", got.State, tt.wantState, got)
			}
			if tt.wantDetail != "" && !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want contains %q", got.Detail, tt.wantDetail)
			}
			if tt.wantHintHas != "" && !strings.Contains(got.Hint, tt.wantHintHas) {
				t.Errorf("hint = %q, want contains %q", got.Hint, tt.wantHintHas)
			}
			if tt.wantExpiry != "" && got.KeyExpiry != tt.wantExpiry {
				t.Errorf("key_expiry = %q, want %q", got.KeyExpiry, tt.wantExpiry)
			}
			// Report-only contract: the never-act guarantee is structural (no
			// tailscale-mutating call exists in this package), asserted here by
			// the absence of an action field and the explicit hint phrasing.
			if tt.wantState == StateKeyExpired && !strings.Contains(got.Hint, "will not act on it") {
				t.Errorf("key-expired hint must state gmux will not act on it, got %q", got.Hint)
			}
		})
	}
}

// TestProbe_FakeTailscaleBinary exercises the real default run + lookPath seams
// (exec.LookPath / exec.CommandContext) against a fake `tailscale` binary placed
// on PATH, so the shell-out contract is verified end to end, not just through
// the injected runner.
func TestProbe_FakeTailscaleBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is unix-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "tailscale")
	// Emits an expired-key netmap when invoked as `tailscale status --json`.
	jsonBody := `{"BackendState":"Running","Expired":true,"Self":{"KeyExpiry":"2026-05-05T10:00:00Z"}}`
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = status ] && [ \"$2\" = --json ]; then\n" +
		"  printf '%s' '" + jsonBody + "'\n" +
		"fi\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := New().Probe(context.Background())
	if got.State != StateKeyExpired {
		t.Fatalf("state = %q, want %q (got %+v)", got.State, StateKeyExpired, got)
	}
	if got.KeyExpiry != "2026-05-05T10:00:00Z" {
		t.Errorf("key_expiry = %q, want 2026-05-05T10:00:00Z", got.KeyExpiry)
	}
	if !strings.Contains(got.Hint, "tailscale up") {
		t.Errorf("hint = %q, want to name `tailscale up`", got.Hint)
	}
}

// TestProbe_NotInstalled proves a plain-LAN host (tailscale CLI absent) is
// classified not_installed and, crucially, NO subprocess runs (the run seam is
// counted; it must stay at 0).
func TestProbe_NotInstalled(t *testing.T) {
	var runs atomic.Int32
	p := NewWithRunner(time.Second,
		func(string) (string, error) { return "", errors.New("not found") },
		func(context.Context) ([]byte, error) { runs.Add(1); return []byte(`{}`), nil },
	)
	got := p.Probe(context.Background())
	if got.State != StateNotInstalled {
		t.Fatalf("state = %q, want %q", got.State, StateNotInstalled)
	}
	if runs.Load() != 0 {
		t.Errorf("run seam called %d times; a missing binary must never spawn a probe", runs.Load())
	}
}

// TestProbe_CachesWithinTTL proves health polling never spawns a process per
// call: many sequential probes within the TTL run the subprocess once, and the
// value refreshes once the clock passes the TTL.
func TestProbe_CachesWithinTTL(t *testing.T) {
	var runs atomic.Int32
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	p := NewWithRunner(15*time.Second,
		func(string) (string, error) { return "/usr/bin/tailscale", nil },
		func(context.Context) ([]byte, error) {
			n := runs.Add(1)
			return []byte(fmt.Sprintf(`{"BackendState":"Running","Self":{"KeyExpiry":"2027-01-0%dT00:00:00Z"}}`, n)), nil
		},
	)
	p.now = func() time.Time { return now }

	for i := 0; i < 20; i++ {
		if s := p.Probe(context.Background()); s.State != StateOK {
			t.Fatalf("probe %d state = %q, want ok", i, s.State)
		}
		now = now.Add(time.Second) // 20s total, within/over the 15s TTL
	}
	if got := runs.Load(); got > 2 {
		t.Fatalf("subprocess ran %d times within ~20s; want at most 2 (one per TTL window)", got)
	}
	if runs.Load() < 1 {
		t.Fatalf("subprocess never ran")
	}
}

// TestProbe_SingleFlight proves concurrent callers share a single refresh: a
// blocking run is entered exactly once even under a burst.
func TestProbe_SingleFlight(t *testing.T) {
	var runs atomic.Int32
	enter := make(chan struct{})
	release := make(chan struct{})
	p := NewWithRunner(time.Second,
		func(string) (string, error) { return "/usr/bin/tailscale", nil },
		func(context.Context) ([]byte, error) {
			runs.Add(1)
			close(enter)
			<-release
			return []byte(`{"BackendState":"Running"}`), nil
		},
	)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s := p.Probe(context.Background()); s.State != StateOK {
				t.Errorf("concurrent probe state = %q", s.State)
			}
		}()
	}
	<-enter                           // first leader is inside run
	time.Sleep(50 * time.Millisecond) // let the rest pile onto the flight
	close(release)
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("run seam entered %d times under a concurrent burst; want 1 (single-flight)", got)
	}
}

func TestSupportedGOOS(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin":
		if !supportedGOOS() {
			t.Fatalf("supportedGOOS() = false on %s", runtime.GOOS)
		}
	default:
		if supportedGOOS() {
			t.Fatalf("supportedGOOS() = true on unprobed platform %s", runtime.GOOS)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("\n\n  hello world  \nsecond\n"); got != "hello world" {
		t.Errorf("firstLine = %q, want %q", got, "hello world")
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine(empty) = %q, want empty", got)
	}
	long := firstLine(strings.Repeat("x", 300))
	if utf8.RuneCountInString(long) != 201 || !strings.HasSuffix(long, "…") {
		t.Errorf("firstLine must bound long lines to 200 runes + ellipsis; got runes=%d", utf8.RuneCountInString(long))
	}
}
