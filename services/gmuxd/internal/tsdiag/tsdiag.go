// Package tsdiag reports, read-only, whether the local tailscale node can
// reach the tailnet at all — the layer beneath peer link dials.
//
// Why this exists: when the LOCAL tailscale node key expires (or tailscaled is
// down / logged out), every peer link dial to a tailscale IP fails identically
// and forever, and the peering layer sees only a generic `i/o timeout`
// ("connection timed out"). That is indistinguishable, at the dial layer, from a
// single peer being down. The answer to "is upstream access unavailable, and
// why?" lives in tailscaled, not in the dials.
//
// The prober shells out to the system `tailscale status --json` (the tailscaled
// the host dials peers THROUGH — gmux's embedded tsnet listener, when present,
// is a separate node and does not reflect system-tailscaled key expiry). It
// inspects the local node's Expired flag, KeyExpiry, and BackendState.
//
// It is strictly observational: it NEVER runs `tailscale up`, re-authenticates,
// logs out, or mutates tailscale state in any way. Re-auth is operator-level and
// must stay operator-level. The prober only reports a diagnosis and a hint.
//
// It guards the environments where a shell-out is pointless or wrong:
//   - non-linux/darwin platforms (windows, …): not probed (`unsupported`);
//   - tailscale CLI absent (plain-LAN peers, containers): not probed
//     (`not_installed`) — not an outage, just not applicable;
//   - tailscaled down / LocalAPI unreachable: classified `tailscale_not_running`.
//
// A probe that FAILED TO PRODUCE EVIDENCE (timeout, killed, unparseable output)
// is classified `probe_failed`, never `tailscale_not_running`: a probe we could
// not complete tells us nothing about tailscaled, and must not be presented to
// the operator as an outage with a re-authentication hint. Only positive
// classifications carry advisory `Hint` text.
//
// The `tailscale status --json` subprocess is the only practical way to read the
// system tailscaled LocalAPI without adding tailscale.com's ipnclient plumbing
// (which assumes a specific socket layout). To keep the hot status/health path
// cheap, results are cached with a short TTL and refreshed under single-flight,
// so per-request health/world renders never spawn a process.
package tsdiag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// State is the outcome of a local tailscale probe.
type State string

const (
	// StateUnknown: not probed yet, or a probe is still in flight and the
	// caller's bound on it expired. Not an outage; carries no advice.
	StateUnknown State = "unknown"
	// StateOK: tailscale present, tailscaled running, local key valid.
	StateOK State = "ok"
	// StateKeyExpired: the LOCAL node key has expired. Dials to peers cannot
	// succeed. The operator must re-authenticate (gmux only reports this).
	StateKeyExpired State = "key_expired"
	// StateNotRunning: tailscaled is positively known not to serve peers —
	// it answered with NeedsLogin / NoState / Stopped / no backend state, or
	// the CLI itself reported it cannot reach the LocalAPI socket.
	StateNotRunning State = "tailscale_not_running"
	// StateProbeFailed: the probe could not complete (exec failed, timed out,
	// was killed, returned nothing, returned output we could not parse). This
	// is deliberately NOT tailscale_not_running: an inconclusive probe must
	// never be translated into operator advice. It carries no Hint.
	StateProbeFailed State = "probe_failed"
	// StateNotInstalled: the tailscale CLI is absent. Plain-LAN peers and
	// containers land here; this is NOT an outage.
	StateNotInstalled State = "not_installed"
	// StateUnsupported: the platform is not probed (non-linux/darwin).
	StateUnsupported State = "unsupported"
)

// Status is the report a probe yields.
type Status struct {
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Hint is the operator remediation, present ONLY when the state is a
	// positive classification (key expiry, tailscaled not serving). gmux
	// NEVER runs it.
	Hint string `json:"hint,omitempty"`
	// BackendState is tailscaled's backend state string when known (Running,
	// NeedsLogin, NoState, Starting, …).
	BackendState string `json:"backend_state,omitempty"`
	// KeyExpiry is the local node key expiry (RFC3339) when known.
	KeyExpiry string `json:"key_expiry,omitempty"`
}

const (
	// statusTTL bounds how stale a positive (evidence-backed) probe may read
	// before a health/status render triggers a refresh. Short enough that a
	// freshly-expired key is visible within a status poll; long enough that
	// repeated polls never spawn a process.
	statusTTL = 15 * time.Second
	// inconclusiveTTL caches a probe that produced NO evidence for far less
	// than statusTTL: fail fast, re-probe soon. Caching a false "not running"
	// verdict for a full 15s window would advertise a phantom outage (and its
	// re-auth advice) to every poll in it.
	inconclusiveTTL = 2 * time.Second
	// probeTimeout bounds a single `tailscale status --json` invocation.
	// tailscaled can stall (starting, unresponsive socket), so the shell-out
	// is time-boxed and classified, never allowed to hang the health path.
	// Enforced as a real WALL-CLOCK budget, including the drain of the
	// child's pipes (see execTailscaleStatus: process-group kill + WaitDelay).
	probeTimeout = 3 * time.Second
	// probeGrace is the extra time Cmd.WaitDelay allows the pipes to drain
	// AFTER the process group has been killed, so a forking wrapper shim
	// cannot keep the read open in a grandchild.
	probeGrace = 500 * time.Millisecond
	// waiterTimeout bounds how long a caller that joined an in-flight probe
	// waits for it. Callers pass context.Background() (the health handler and
	// the world composer have no request-scoped deadline to inherit), so the
	// bound is the probe's own wall-clock budget plus slack — a wedged probe
	// must never block the health handler or the composer unboundedly.
	waiterTimeout = probeTimeout + probeGrace + time.Second
	// maxProbeOutput caps the subprocess output retained. A netmap JSON is
	// bounded in practice; a hostile/wedged `tailscale` must not be able to
	// balloon the daemon's heap through the health path.
	maxProbeOutput = 1 << 20
)

// errProbeOutputCapped marks a probe whose output exceeded maxProbeOutput. The
// retained bytes are a meaningless prefix, so the result is classified
// inconclusive, never as a tailscaled verdict. Defined here (portable) because
// the exec layer that produces it is platform-specific.
var errProbeOutputCapped = errors.New("tailscale status output exceeded the probe output cap")

// Prober reads local tailscale status with a short-lived cache and single-flight.
//
// run and lookPath are seams for tests: run shells out to `tailscale status
// --json` (returning combined stdout/stderr plus the exec error), lookPath
// resolves the binary. Both are nil in production, which selects the defaults.
type Prober struct {
	run      func(ctx context.Context) ([]byte, error)
	lookPath func(string) (string, error)
	ttl      time.Duration
	now      func() time.Time

	mu       sync.Mutex
	cached   Status
	cachedAt time.Time
	probed   bool  // a probe (or install check) has completed at least once
	inflight *call // single-flight for concurrent refreshes
}

type call struct {
	done chan struct{}
	res  Status
}

// New returns a Prober using the system `tailscale` binary.
func New() *Prober {
	return &Prober{ttl: statusTTL}
}

// NewWithRunner builds a Prober over injected seams (tests). ttl <= 0 selects
// the default TTL.
func NewWithRunner(ttl time.Duration, lookPath func(string) (string, error), run func(context.Context) ([]byte, error)) *Prober {
	if ttl <= 0 {
		ttl = statusTTL
	}
	return &Prober{run: run, lookPath: lookPath, ttl: ttl}
}

func (p *Prober) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Prober) resolvePath() (string, error) {
	if p.lookPath != nil {
		return p.lookPath("tailscale")
	}
	return exec.LookPath("tailscale")
}

func (p *Prober) execStatus(ctx context.Context) ([]byte, error) {
	if p.run != nil {
		return p.run(ctx)
	}
	return execTailscaleStatus(ctx)
}

// cacheTTL is the freshness window for a cached result. Positive
// classifications are stable facts about tailscaled and ride the full TTL;
// inconclusive results are re-probed quickly so a transient/wedged probe is
// never advertised for a full window.
func cacheTTL(state State, ttl time.Duration) time.Duration {
	if !state.isPositive() {
		if inconclusiveTTL < ttl {
			return inconclusiveTTL
		}
	}
	return ttl
}

// isPositive reports whether a state is backed by evidence from tailscaled (or
// by a definitive environment fact), as opposed to "the probe could not tell
// us". Only positive classifications carry advisory text and the full cache TTL.
func (s State) isPositive() bool {
	switch s {
	case StateOK, StateKeyExpired, StateNotRunning, StateNotInstalled, StateUnsupported:
		return true
	default:
		return false
	}
}

// supportedGOOS reports whether this platform runs the tailscale CLI in a way
// we probe. Key expiry is cross-platform, but the shell-out + socket shapes are
// best-verified on linux/darwin; everywhere else we decline (no exec) rather
// than risk a spurious "not_running" on an environment we haven't audited. It
// also keeps the process-group exec (which needs Setpgid) off platforms that
// do not have it.
func supportedGOOS() bool {
	switch runtime.GOOS {
	case "linux", "darwin":
		return true
	default:
		return false
	}
}

// Probe returns the local tailscale status, refreshing it when the cache is
// stale. It is safe for concurrent use and cheap on the cache: at most one
// subprocess runs per TTL window, and concurrent callers share its result.
//
// Probe never blocks longer than its own budget: the leader is bounded by
// probeTimeout (+ grace) and waiters by waiterTimeout, after which they get
// StateUnknown ("probe in progress") and render no upstream-access diagnostic.
func (p *Prober) Probe(ctx context.Context) Status {
	// Non-probed platforms and a missing binary are decided without a
	// subprocess, so a plain-LAN host pays nothing per call.
	if !supportedGOOS() {
		return Status{State: StateUnsupported, Detail: "tailscale status is not probed on this platform"}
	}
	if _, err := p.resolvePath(); err != nil {
		return Status{State: StateNotInstalled, Detail: "tailscale CLI not found (plain-LAN peers do not need it)"}
	}

	now := p.clock()
	p.mu.Lock()
	if p.probed && now.Sub(p.cachedAt) <= cacheTTL(p.cached.State, p.ttl) {
		res := p.cached
		p.mu.Unlock()
		return res
	}
	if p.inflight != nil {
		c := p.inflight
		p.mu.Unlock()
		// A waiter must not be able to outlive the probe it joined: callers
		// pass context.Background(), so ctx.Done() alone is not a bound.
		timer := time.NewTimer(p.budget())
		defer timer.Stop()
		select {
		case <-c.done:
			return c.res
		case <-ctx.Done():
			return Status{State: StateUnknown, Detail: "probe cancelled"}
		case <-timer.C:
			return Status{State: StateUnknown, Detail: "probe in progress"}
		}
	}
	c := &call{done: make(chan struct{})}
	p.inflight = c
	p.mu.Unlock()

	start := p.clock()
	res := p.runProbe(ctx)
	took := p.clock().Sub(start)

	p.mu.Lock()
	prev, hadPrev := p.cached.State, p.probed
	p.cached = res
	p.cachedAt = p.clock()
	p.probed = true
	p.inflight = nil
	c.res = res
	p.mu.Unlock()
	close(c.done)

	logProbe(res, prev, hadPrev, took)
	return res
}

// budget is the caller-side bound: the probe's wall-clock budget plus the
// grace the exec layer allows for pipe drain.
func (p *Prober) budget() time.Duration { return waiterTimeout }

// ProbeBudget is the deadline a health/status caller should give Probe. The
// probe is single-flight, so a caller may be a WAITER on someone else's probe:
// a context.Background() caller would then have no bound at all. Deriving the
// context with this budget makes "health never blocks on tailscale" a property
// of the call site, not just of the prober.
func ProbeBudget() time.Duration { return waiterTimeout }

// runProbe executes one probe and classifies the outcome. Every branch that
// cannot positively attribute the failure to tailscaled returns StateProbeFailed
// with an empty Hint — the daemon must never render operator advice ("run
// tailscale up") for evidence it does not have.
func (p *Prober) runProbe(ctx context.Context) Status {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := p.execStatus(probeCtx)

	// The probe not completing is never a tailscaled verdict: a deadline, a
	// kill, an exec failure, or a capped (truncated) output means "we could not
	// tell", not "down".
	if probeCtx.Err() != nil {
		return probeFailed(fmt.Sprintf("probe timed out after %s", probeTimeout), err)
	}
	if isSignalExit(err) {
		// Terminated by a signal — our own group kill is the common case. The
		// exec error text ("signal: killed") is a fact about OUR probe, and
		// must never be rendered as a tailscaled outage with a re-auth hint.
		return probeFailed("tailscale status was terminated by a signal", err)
	}
	if errors.Is(err, errProbeOutputCapped) {
		return probeFailed("tailscale status output was truncated", nil)
	}
	// A non-ExitError means the binary could not be run at all (permission,
	// fork failure) — unless it also produced output, which proves it ran. The
	// injected test seam returns plain errors for "exit status 1", so the
	// classification keys on the evidence (output printed), not on the concrete
	// error type.
	probeRan := isExitStatus(err) || len(strings.TrimSpace(string(out))) > 0
	if err != nil && !probeRan {
		return probeFailed(fmt.Sprintf("probe could not run: %v", err), nil)
	}

	var st struct {
		BackendState string `json:"BackendState"`
		Expired      bool   `json:"Expired"`
		AuthURL      string `json:"AuthURL"`
		Self         struct {
			KeyExpiry *time.Time `json:"KeyExpiry"`
		} `json:"Self"`
	}
	if jsonErr := json.Unmarshal(out, &st); jsonErr != nil {
		detail := firstLine(string(out))
		if detail == "" {
			// No output at all: an ExitError with nothing printed, or an
			// output-too-large truncation that ate the JSON. We cannot tell
			// tailscaled down from "CLI printed nothing".
			return probeFailed("tailscale status returned no usable output", err)
		}
		if json.Valid(out) {
			// Well-formed JSON that we could not decode (a field changed type,
			// a future shape): tailscaled answered, so this is a contract break
			// on OUR side, not an outage. Report it, advise nothing.
			return probeFailed(fmt.Sprintf("tailscale status JSON did not match the expected shape: %v", jsonErr), nil)
		}
		// The CLI ran, printed a real error, and exited: positive evidence it
		// cannot reach tailscaled (the classic "Failed to connect to local IPN
		// socket"). This is the one non-JSON path that may advise.
		return Status{
			State:  StateNotRunning,
			Detail: fmt.Sprintf("tailscale status unavailable: %s", detail),
			Hint:   "start tailscaled and log in: `tailscale up` (gmux only reports this; it will not act on it)",
		}
	}

	// Report the local key expiry whenever tailscaled reports it, in every
	// branch, so status shows when the key is due (and, on expiry, when it
	// died).
	expiry := ""
	if st.Self.KeyExpiry != nil {
		expiry = st.Self.KeyExpiry.UTC().Format(time.RFC3339)
	}

	switch {
	case st.Expired:
		return Status{
			State:        StateKeyExpired,
			BackendState: st.BackendState,
			KeyExpiry:    expiry,
			Detail:       fmt.Sprintf("local tailscale node key EXPIRED%s — dials to peers cannot succeed", atExpiry(expiry)),
			Hint:         "run `tailscale up` to re-authenticate (gmux only reports this; it will not act on it)",
		}
	case st.BackendState == "Running":
		detail := ""
		if expiry != "" {
			detail = fmt.Sprintf("tailscale key valid until %s", expiry)
		}
		return Status{State: StateOK, Detail: detail, BackendState: st.BackendState, KeyExpiry: expiry}
	case st.BackendState == "":
		// JSON parsed but no backend state: tailscaled reachable but stateless.
		return Status{
			State:  StateNotRunning,
			Detail: "tailscale backend state is unknown (tailscaled not fully up)",
			Hint:   "check tailscaled: `tailscale status` (gmux only reports this; it will not act on it)",
		}
	default:
		// NeedsLogin / NoState / Starting / Stopped: not serving peers.
		return Status{
			State:        StateNotRunning,
			BackendState: st.BackendState,
			KeyExpiry:    expiry,
			Detail:       fmt.Sprintf("tailscale backend is %s — peers are unreachable", st.BackendState),
			Hint:         "log in / start tailscale: `tailscale up` (gmux only reports this; it will not act on it)",
		}
	}
}

// probeFailed builds the non-alerting inconclusive result. It carries no Hint
// by construction: the type system cannot express advice for "we don't know",
// so a wedged probe can never be rendered as a re-auth instruction.
func probeFailed(detail string, cause error) Status {
	if cause != nil {
		detail = fmt.Sprintf("%s (%v)", detail, cause)
	}
	return Status{
		State:  StateProbeFailed,
		Detail: fmt.Sprintf("unavailable (probe failed: %s)", detail),
	}
}

// atExpiry renders " (expired <ts>)" for a diagnostic when tailscaled reported
// an expiry — an operator triaging at 3am needs to know WHEN it died.
func atExpiry(expiry string) string {
	if expiry == "" {
		return ""
	}
	return fmt.Sprintf(" (%s)", expiry)
}

// isExitStatus reports whether err is the "command ran and exited" error.
func isExitStatus(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// isSignalExit reports whether the command was terminated by a signal — the
// signature of our own timeout kill, not of tailscaled refusing service.
func isSignalExit(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	return ee.ExitCode() < 0 // -1: terminated by a signal
}

// debugEnabled gates the debug-level classification-transition log. The probe
// runs on the health path; a transition log is the only way to correlate a
// wedged probe in the daemon log, and it must not spam healthy hubs.
var debugEnabled = func() bool { return os.Getenv("GMUX_TSDEBUG") != "" }

// logProbe records probe outcomes: every probe that failed to produce evidence
// is logged (rate-limited by the cache TTL, so at most one line per re-probe),
// and state transitions are logged at debug level.
func logProbe(res Status, prev State, hadPrev bool, took time.Duration) {
	// Inconclusive and slow probes are logged unconditionally: they are the
	// only trace of a wedged tailscale CLI in the daemon log, and the cache TTL
	// bounds the rate (at most one line per re-probe).
	if res.State == StateProbeFailed {
		log.Printf("tsdiag: probe took %s and produced no verdict: %s", took.Round(time.Millisecond), res.Detail)
	} else if took >= probeTimeout {
		log.Printf("tsdiag: probe took %s (at the %s budget): %s", took.Round(time.Millisecond), probeTimeout, res.Detail)
	}
	if debugEnabled() && (!hadPrev || res.State != prev) {
		log.Printf("tsdiag[debug]: %s -> %s (%s, %s)", prev, res.State, res.Detail, took.Round(time.Millisecond))
	}
}

// firstLine returns the first non-empty, trimmed line of s (bounded to 200
// RUNES, so multi-byte text is never split mid-rune), for use as a compact
// failure reason from CLI stderr.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if runes := []rune(line); len(runes) > 200 {
				return string(runes[:200]) + "…"
			}
			return line
		}
	}
	return ""
}
