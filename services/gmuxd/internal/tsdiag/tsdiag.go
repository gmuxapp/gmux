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
// The `tailscale status --json` subprocess is the only practical way to read the
// system tailscaled LocalAPI without adding tailscale.com's ipnclient plumbing
// (which assumes a specific socket layout). To keep the hot status/health path
// cheap, results are cached with a short TTL and refreshed under single-flight,
// so per-request health/world renders never spawn a process.
package tsdiag

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// State is the outcome of a local tailscale probe.
type State string

const (
	// StateUnknown: not probed (nothing needed it yet).
	StateUnknown State = "unknown"
	// StateOK: tailscale present, tailscaled running, local key valid.
	StateOK State = "ok"
	// StateKeyExpired: the LOCAL node key has expired. Dials to peers cannot
	// succeed. The operator must re-authenticate (gmux only reports this).
	StateKeyExpired State = "key_expired"
	// StateNotRunning: tailscaled is down, logged out (NeedsLogin/NoState),
	// or otherwise not serving peers.
	StateNotRunning State = "tailscale_not_running"
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
	// Hint is the operator remediation, present when the state is actionable
	// (e.g. key expiry). gmux NEVER runs it.
	Hint string `json:"hint,omitempty"`
	// BackendState is tailscaled's backend state string when known (Running,
	// NeedsLogin, NoState, Starting, …).
	BackendState string `json:"backend_state,omitempty"`
	// KeyExpiry is the local node key expiry (RFC3339) when known.
	KeyExpiry string `json:"key_expiry,omitempty"`
}

// statusTTL bounds how stale a cached probe may read before a health/status
// render triggers a refresh. Short enough that a freshly-expired key is visible
// within a status poll; long enough that repeated polls never spawn a process.
const statusTTL = 15 * time.Second

// probeTimeout bounds a single `tailscale status --json` invocation. tailscaled
// can stall (starting, unresponsive socket), so the shell-out is time-boxed and
// classified, never allowed to hang the health path.
const probeTimeout = 3 * time.Second

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
	cmd := exec.CommandContext(ctx, "tailscale", "status", "--json")
	return cmd.CombinedOutput()
}

// supportedGOOS reports whether this platform runs the tailscale CLI in a way
// we probe. Key expiry is cross-platform, but the shell-out + socket shapes are
// best-verified on linux/darwin; everywhere else we decline (no exec) rather
// than risk a spurious "not_running" on an environment we haven't audited.
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
	if p.probed && now.Sub(p.cachedAt) <= p.ttl {
		res := p.cached
		p.mu.Unlock()
		return res
	}
	if p.inflight != nil {
		c := p.inflight
		p.mu.Unlock()
		select {
		case <-c.done:
			return c.res
		case <-ctx.Done():
			return Status{State: StateUnknown, Detail: "probe cancelled"}
		}
	}
	c := &call{done: make(chan struct{})}
	p.inflight = c
	p.mu.Unlock()

	res := p.runProbe(ctx)

	p.mu.Lock()
	p.cached = res
	p.cachedAt = p.clock()
	p.probed = true
	p.inflight = nil
	c.res = res
	p.mu.Unlock()
	close(c.done)
	return res
}

func (p *Prober) runProbe(ctx context.Context) Status {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := p.execStatus(probeCtx)

	var st struct {
		BackendState string `json:"BackendState"`
		Expired      bool   `json:"Expired"`
		AuthURL      string `json:"AuthURL"`
		Self         struct {
			KeyExpiry *time.Time `json:"KeyExpiry"`
		} `json:"Self"`
	}
	if jsonErr := json.Unmarshal(out, &st); jsonErr != nil {
		// Not JSON: the CLI failed (tailscaled down, LocalAPI socket missing,
		// permission denied). Report the first output line as the reason. This
		// covers the exec error being nil (CLI ran, printed an error, exit 0 is
		// rare) and being non-nil alike — both mean "not usable".
		detail := firstLine(string(out))
		if detail == "" {
			if err != nil {
				detail = err.Error()
			} else {
				detail = "tailscale status returned no usable output"
			}
		}
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
			Detail:       "local tailscale node key EXPIRED — dials to peers cannot succeed",
			Hint:         "run `tailscale up` to re-authenticate (gmux only reports this; it will not act on it)",
		}
	case st.BackendState == "Running":
		return Status{State: StateOK, BackendState: st.BackendState, KeyExpiry: expiry}
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

// firstLine returns the first non-empty, trimmed line of s (bounded), for use
// as a compact failure reason from CLI stderr.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			return line
		}
	}
	return ""
}
