package tsdiag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// unixOnly skips tests that need a shell fake and process groups.
func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script and process groups are unix-only")
	}
}

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
			// Positive evidence: the CLI RAN, printed a real LocalAPI failure,
			// and exited. This is the one non-JSON path allowed to advise.
			name:        "tailscaled down (non-JSON output + exec error)",
			out:         badOutput,
			runErr:      errors.New("exit status 1"),
			wantState:   StateNotRunning,
			wantHintHas: "tailscale up",
			wantDetail:  "Failed to connect to local IPN socket",
		},
		{
			// F2: our OWN timeout kill reported as "signal: killed" must never
			// become a tailscaled outage with re-auth advice.
			name:       "probe killed by signal (inconclusive)",
			runErr:     errors.New("signal: killed"),
			wantState:  StateProbeFailed,
			wantDetail: "probe failed",
		},
		{
			name:       "probe timed out with no output (inconclusive)",
			runErr:     context.DeadlineExceeded,
			wantState:  StateProbeFailed,
			wantDetail: "probe failed",
		},
		{
			name:       "CLI printed nothing (inconclusive)",
			runErr:     errors.New("exit status 1"),
			wantState:  StateProbeFailed,
			wantDetail: "probe failed",
		},
		{
			// Well-formed JSON we cannot decode = tailscaled ANSWERED, so this
			// is a contract break on our side, not an outage.
			name:       "JSON with Expired typed as a string (inconclusive)",
			out:        `{"BackendState":"Running","Expired":"yes"}`,
			wantState:  StateProbeFailed,
			wantDetail: "expected shape",
		},
		{
			name:       "BOM-prefixed JSON (inconclusive)",
			out:        "\xef\xbb\xbf{\"BackendState\":\"Running\"}",
			wantState:  StateProbeFailed,
			wantDetail: "probe failed",
		},
		{
			// F10: an operator triaging at 3am needs to know WHEN it died.
			name:       "healthy with expiry reported",
			out:        `{"BackendState":"Running","Self":{"KeyExpiry":"2027-02-16T05:18:54Z"}}`,
			wantState:  StateOK,
			wantDetail: "2027-02-16T05:18:54Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewWithRunner(time.Second, nil, func(ctx context.Context) ([]byte, error) {
				if tt.runErr != nil && strings.Contains(tt.runErr.Error(), "deadline") {
					<-ctx.Done() // the seam honours the probe deadline like exec does
				}
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
			// The core F2 contract, asserted on EVERY row: advice exists only
			// for positive classifications. An inconclusive probe must never
			// carry the re-auth instruction (mutant-proof: a regression that
			// re-attaches a Hint to probe_failed fails here, not in prod).
			if got.State == StateProbeFailed && got.Hint != "" {
				t.Errorf("inconclusive probe carries advice %q; only positive classifications may", got.Hint)
			}
			if !got.State.isPositive() && got.Hint != "" {
				t.Errorf("state %q is not positive but carries advice %q", got.State, got.Hint)
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
// TestProbe_ForkingTailscaleIsKilledAtDeadline is the F1 regression: a
// `tailscale` that FORKS — the shape of every site-local wrapper shim (`sudo
// tailscale "$@"`, a non-exec Nix/Guix wrapper, a shim that logs) — must not be
// able to extend the probe past its budget by holding the stdout pipe open in a
// grandchild. Killing only the direct child left the read blocked for the
// grandchild's lifetime: measured 33.7s for a documented 3s probe, stalling
// /v1/health, the browser's first world render, and the snapshot composer.
//
// Asserts both halves of the fix: the probe returns within ~probeTimeout, and
// the whole process group is gone afterwards (no orphaned sleeps).
func TestProbe_ForkingTailscaleIsKilledAtDeadline(t *testing.T) {
	unixOnly(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	fake := filepath.Join(dir, "tailscale")
	// Long enough that the fake's own lifetime (30s) is unambiguously longer
	// than the probe budget, short enough that a failure leaks for seconds, not
	// minutes. Records EVERY pid it spawns so the test kills only its own.
	// Every spawned pid is recorded, so cleanup touches only this fake's own
	// processes. The trailing `wait` keeps the shim alive (and its stdout, i.e.
	// the pipe, open) while the grandchildren run — the exact shim shape that
	// defeated a direct-child-only kill.
	script := "#!/bin/sh\n" +
		"echo $$ >> " + pidFile + "\n" +
		"sleep 30 & echo $! >> " + pidFile + "\n" +
		"sleep 30 & echo $! >> " + pidFile + "\n" +
		"wait\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Cleanup(func() {
		// Kill ONLY the pids this fake recorded for itself, never by name.
		for _, pid := range readPIDs(t, pidFile) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	start := time.Now()
	got := New().Probe(context.Background())
	elapsed := time.Since(start)

	if elapsed > probeTimeout+probeGrace+500*time.Millisecond {
		t.Fatalf("probe took %s; the documented budget is %s (wall clock, including a forking child)",
			elapsed.Round(time.Millisecond), probeTimeout)
	}
	// Our kill must not be reported as a tailscaled outage (F2), and must not
	// advise `tailscale up`.
	if got.State != StateProbeFailed {
		t.Fatalf("state = %q, want probe_failed (got %+v)", got.State, got)
	}
	if got.Hint != "" {
		t.Errorf("inconclusive probe carries advice %q", got.Hint)
	}

	// The group kill must have reached the grandchild that held the pipe.
	deadline := time.Now().Add(3 * time.Second)
	for {
		live := livePIDs(readPIDs(t, pidFile))
		if len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe left processes alive after its deadline: %v", live)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // the fake had not started yet
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// livePIDs drops pids that are gone. A zombie counts as live: it has not been
// reaped (the test's own children are, by exec).
func livePIDs(pids []int) []int {
	var live []int
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); err == nil {
			live = append(live, pid)
		}
	}
	return live
}

// TestProbe_OutputIsCapped proves the subprocess output read is bounded: a
// `tailscale` printing far more than the cap must not balloon the daemon's heap
// through the health path (a 256 MiB fake cost +1.07 GiB heap unbounded), and
// the truncated result is classified inconclusive, never as a tailscaled verdict
// decoded from a meaningless prefix.
func TestProbe_OutputIsCapped(t *testing.T) {
	unixOnly(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "tailscale")
	script := "#!/bin/sh\nhead -c 4000000 /dev/zero | tr '\\\\0' 'x'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := New().Probe(context.Background())
	if got.State != StateProbeFailed {
		t.Fatalf("state = %q, want probe_failed (capped output is not a verdict)", got.State)
	}
	if got.Hint != "" {
		t.Errorf("capped probe carries advice %q", got.Hint)
	}
}

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

// TestProbe_CachesWithinTTL proves the cache is bounded BY the TTL in both
// directions: within the window the subprocess runs EXACTLY once (a permanently
// cached prober would also pass a ">= 1" assertion), and after the window it
// runs again AND its new value is what gets served. Asserting only
// "1 <= runs <= 2" cannot tell "cached within TTL" from "cached forever".
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

	// 14 polls inside the 15s window: one subprocess, and the FIRST value
	// served every time.
	for i := 0; i < 14; i++ {
		if s := p.Probe(context.Background()); s.State != StateOK {
			t.Fatalf("probe %d state = %q, want ok", i, s.State)
		}
		if s := p.Probe(context.Background()); s.KeyExpiry != "2027-01-01T00:00:00Z" {
			t.Fatalf("probe %d served %q; a cached probe must serve the first value", i, s.KeyExpiry)
		}
		now = now.Add(time.Second)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("subprocess ran %d times within the TTL; want exactly 1", got)
	}

	// Cross the TTL: the cache must expire, re-run, and publish the NEW value.
	now = now.Add(2 * time.Second)
	if s := p.Probe(context.Background()); s.KeyExpiry != "2027-01-02T00:00:00Z" {
		t.Fatalf("after the TTL the probe served %q; want the refreshed value", s.KeyExpiry)
	}
	if got := runs.Load(); got < 2 {
		t.Fatalf("subprocess ran %d times across the TTL boundary; want >= 2", got)
	}
}

// TestProbe_InconclusiveCachesShorterThanPositive pins the split cache policy:
// an evidence-backed classification rides the full TTL, an inconclusive one is
// re-probed quickly. A wedged probe cached for 15s advertises "unavailable"
// (and, before F2, re-auth advice) to every poll in the window.
func TestProbe_InconclusiveCachesShorterThanPositive(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("positive cached for the full TTL", func(t *testing.T) {
		var runs atomic.Int32
		now := base
		p := NewWithRunner(15*time.Second,
			func(string) (string, error) { return "/usr/bin/tailscale", nil },
			func(context.Context) ([]byte, error) {
				runs.Add(1)
				return []byte(`{"BackendState":"NeedsLogin"}`), nil // positive: not running
			},
		)
		p.now = func() time.Time { return now }
		for i := 0; i < 10; i++ {
			if s := p.Probe(context.Background()); s.State != StateNotRunning {
				t.Fatalf("state = %q, want tailscale_not_running", s.State)
			}
			now = now.Add(time.Second)
		}
		if got := runs.Load(); got != 1 {
			t.Fatalf("positive probe ran %d times in 10s; want 1 (cached for the full TTL)", got)
		}
	})

	t.Run("inconclusive re-probed within 2s", func(t *testing.T) {
		var runs atomic.Int32
		now := base
		p := NewWithRunner(15*time.Second,
			func(string) (string, error) { return "/usr/bin/tailscale", nil },
			func(context.Context) ([]byte, error) {
				runs.Add(1)
				return nil, errors.New("signal: killed")
			},
		)
		p.now = func() time.Time { return now }
		for i := 0; i < 10; i++ {
			if s := p.Probe(context.Background()); s.State != StateProbeFailed {
				t.Fatalf("state = %q, want probe_failed", s.State)
			}
			now = now.Add(time.Second)
		}
		// A 15s cache gives exactly 1 run in 10s; the 2s inconclusive cache
		// re-probes at 0/3/6/9s (the boundary is inclusive). The assertion that
		// matters is "many times, not once": a wedged probe is never advertised
		// for a full TTL window.
		if got := runs.Load(); got < 3 {
			t.Fatalf("inconclusive probe ran %d times in 10s; want several (2s cache, not the 15s TTL)", got)
		}
	})
}

// TestProbe_WedgedLeaderDoesNotBlockWaiters is the F1 waiter bound: the probe is
// single-flight, callers pass context.Background(), and a waiter with only
// ctx.Done() to escape on would queue behind a wedged probe forever — the health
// handler and the world composer both do exactly this. A waiter must give up on
// its own budget and report StateUnknown (which the daemon renders as
// upstream_access: null, i.e. "no diagnostic", never a diagnosis).
func TestProbe_WedgedLeaderDoesNotBlockWaiters(t *testing.T) {
	release := make(chan struct{})
	var closed atomic.Bool
	closeOnce := func() {
		if closed.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(closeOnce) // let the leader finish at test end
	entered := make(chan struct{})
	p := NewWithRunner(time.Second,
		func(string) (string, error) { return "/usr/bin/tailscale", nil },
		func(context.Context) ([]byte, error) {
			close(entered)
			<-release // truly wedged: ignores its own deadline, like a real shim
			return []byte(`{"BackendState":"Running"}`), nil
		},
	)
	p.waiter = 150 * time.Millisecond // production default is probe budget + grace

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer closeOnce()
		_ = p.Probe(context.Background()) // the wedged leader
	}()
	<-entered

	start := time.Now()
	got := p.Probe(context.Background()) // the waiter, context.Background()
	elapsed := time.Since(start)
	closeOnce()  // unblock the leader before asserting
	<-leaderDone // no goroutine outlives the test
	if elapsed > p.budget()+500*time.Millisecond {
		t.Fatalf("waiter blocked %s; must give up at its budget (%s)", elapsed, p.budget())
	}
	if got.State != StateUnknown {
		t.Fatalf("state = %q, want unknown (probe in progress)", got.State)
	}
	if got.Hint != "" {
		t.Errorf("inconclusive waiter carries advice %q", got.Hint)
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
	// F8: the bound is in RUNES. Slicing bytewise produced invalid UTF-8 (and a
	// 203-byte "200 character" line) for any multi-byte CLI message.
	multi := firstLine(strings.Repeat("\u20ac", 300))
	if !utf8.ValidString(multi) {
		t.Errorf("firstLine split a multi-byte rune: %q", multi)
	}
	if n := utf8.RuneCountInString(multi); n != 201 {
		t.Errorf("firstLine rune count = %d, want 201", n)
	}
}

// TestLogProbe is F10: tsdiag used to log nothing at all, so a wedged probe was
// invisible in the daemon log — the only place an operator can correlate a
// health stall. Inconclusive and slow probes log unconditionally; state
// transitions log at debug level (GMUX_TSDEBUG) so a healthy hub stays quiet.
func TestLogProbe(t *testing.T) {
	capture := func(t *testing.T, run func()) string {
		t.Helper()
		var buf syncBuffer
		prev := log.Writer()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(prev) })
		run()
		return buf.String()
	}

	t.Run("inconclusive probe is logged without the debug switch", func(t *testing.T) {
		out := capture(t, func() {
			logProbe(Status{State: StateProbeFailed, Detail: "unavailable (probe failed: signal: killed)"}, StateOK, true, 3*time.Second)
		})
		if !strings.Contains(out, "tsdiag:") || !strings.Contains(out, "probe failed: signal: killed") || !strings.Contains(out, "3s") {
			t.Errorf("inconclusive probe must be logged with its duration and detail, got %q", out)
		}
	})

	t.Run("slow probe is logged even when it produced a verdict", func(t *testing.T) {
		out := capture(t, func() {
			logProbe(Status{State: StateOK}, StateOK, true, probeTimeout)
		})
		if !strings.Contains(out, "budget") {
			t.Errorf("a probe that hit its budget must be logged, got %q", out)
		}
	})

	t.Run("fast healthy probe is silent", func(t *testing.T) {
		out := capture(t, func() {
			logProbe(Status{State: StateOK}, StateOK, true, time.Millisecond)
		})
		if out != "" {
			t.Errorf("a fast, unchanged probe must not log, got %q", out)
		}
	})

	t.Run("transitions are debug-only", func(t *testing.T) {
		out := capture(t, func() {
			logProbe(Status{State: StateKeyExpired, Detail: "EXPIRED"}, StateOK, true, time.Millisecond)
		})
		if out != "" {
			t.Errorf("transitions must be silent without GMUX_TSDEBUG, got %q", out)
		}
		t.Setenv("GMUX_TSDEBUG", "1")
		out = capture(t, func() {
			logProbe(Status{State: StateKeyExpired, Detail: "EXPIRED"}, StateOK, true, time.Millisecond)
		})
		if !strings.Contains(out, "ok -> key_expired") {
			t.Errorf("GMUX_TSDEBUG must log the classification transition, got %q", out)
		}
		out = capture(t, func() {
			logProbe(Status{State: StateKeyExpired, Detail: "EXPIRED"}, StateKeyExpired, true, time.Millisecond)
		})
		if out != "" {
			t.Errorf("an unchanged state must not repeat a transition line, got %q", out)
		}
	})
}

// syncBuffer is a goroutine-safe log sink (the peering package has its own copy
// for its tests; tsdiag is a separate test binary).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
