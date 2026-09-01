package main

// bootstrap_discovery_test.go — mutation-grade tests for stale socket reaping,
// exact-incarnation suppression and transition diagnostics.
//
// These tests use the production runner transport (productionRunnerClient)
// against real Unix sockets in a real socket directory, because the properties
// under test are filesystem properties: what ECONNREFUSED means, what an inode
// is, and what the reaper is allowed to unlink. A fake transport could not
// produce a genuine refusal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// socketDir points both the reaper's trust boundary and the endpoint source at
// an isolated directory.
func socketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", dir)
	return dir
}

// crashedRunnerSocket reproduces the state a runner leaves behind when it dies
// without releasing ownership: the socket pathname still exists and refuses
// connections, and the lock file is still there with nobody holding it (the
// kernel drops flock when the process dies, but never unlinks the file).
func crashedRunnerSocket(t *testing.T, dir, name string) string {
	t.Helper()
	ep := filepath.Join(dir, name)
	ln, err := net.Listen("unix", ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(socklease.LockPath(ep), nil, 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	return ep
}

// liveRunner serves the minimum runner API (an /events stream and /meta) on a
// real Unix socket, so the production transport can register against it.
type liveRunner struct {
	ep          string
	id          centralstore.SessionID
	incarnation string
	ln          net.Listener
	srv         *http.Server
	lease       *socklease.Lease
	closed      sync.Once
}

func startLiveRunner(t *testing.T, dir, name string, id centralstore.SessionID, leased bool) *liveRunner {
	t.Helper()
	ep := filepath.Join(dir, name)
	// A real runner mints an ephemeral incarnation and stamps it on every
	// response; the daemon requires subscription and metadata to agree before
	// it will claim to know which socket a generation is subscribed to.
	r := &liveRunner{ep: ep, id: id, incarnation: fmt.Sprintf("incarnation-%s-%d", name, time.Now().UnixNano())}
	if leased {
		lease, err := socklease.Acquire(ep)
		if err != nil {
			t.Fatalf("acquire lease: %v", err)
		}
		r.lease = lease
	}
	ln, err := net.Listen("unix", ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	r.ln = ln

	mux := http.NewServeMux()
	stamp := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Gmux-Incarnation", r.incarnation)
			next(w, req)
		}
	}
	mux.HandleFunc("GET /events", stamp(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-req.Context().Done()
	}))
	mux.HandleFunc("GET /meta", stamp(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": string(id), "adapter": "shell", "alive": true, "pid": os.Getpid(),
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"incarnation": r.incarnation,
		})
	}))
	r.srv = &http.Server{Handler: mux}
	go func() { _ = r.srv.Serve(ln) }()
	t.Cleanup(r.stop)
	return r
}

func (r *liveRunner) stop() {
	r.closed.Do(func() {
		_ = r.srv.Close()
		if r.lease != nil {
			_ = r.lease.Release()
		}
	})
}

// crash makes the runner die the way a SIGKILL would: the listener goes away,
// the pathname stays, and the lease is dropped without unlinking the lock file.
func (r *liveRunner) crash(t *testing.T) {
	t.Helper()
	_ = r.srv.Close()
	if r.lease != nil {
		// Simulate kernel lock release on process death: the lock file must
		// survive, so write it back after Release unlinks it.
		_ = r.lease.Release()
		if err := os.WriteFile(socklease.LockPath(r.ep), nil, 0o600); err != nil {
			t.Fatalf("restore lock file: %v", err)
		}
		r.lease = nil
	}
}

type discoveryHarness struct {
	boot    *Bootstrap
	store   *centralstore.Store
	errs    *recorder
	notices *recorder
	dir     string
	eps     func() []string
}

type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
}
func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
func (r *recorder) count() int { return len(r.all()) }
func (r *recorder) countContaining(sub string) int {
	n := 0
	for _, l := range r.all() {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func newDiscoveryHarness(t *testing.T) *discoveryHarness {
	t.Helper()
	dir := socketDir(t)
	store, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &discoveryHarness{store: store, errs: &recorder{}, notices: &recorder{}, dir: dir}
	boot, err := newBootstrap(BootstrapConfig{ComposeMinInterval: -1, 
		Store: store, Runners: productionRunnerClient{}, Control: bootstrapControl{}, Spawner: bootstrapSpawner{},
		Reconciler: bootstrapReconciler{}, Converter: &wire.Converter{},
		Endpoints: productionEndpointSource{},
		Errors:    sessioncoord.ErrorSinkFunc(func(_ context.Context, err error) { h.errs.add(err.Error()) }),
		Notices:   func(_ context.Context, msg string) { h.notices.add(msg) },
		Clock:     func() centralstore.UnixMillis { return 100 },
		// Keep a failed dial cheap: these tests deliberately probe dead sockets.
		RunnerBudget: 2 * time.Second, ConvergeDeadline: 5 * time.Second,
		RetryInitial: time.Millisecond, RetryMaximum: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(boot.Close)
	h.boot = boot
	return h
}

func (h *discoveryHarness) converge(t *testing.T) {
	t.Helper()
	if _, err := h.boot.Converge(context.Background()); err != nil {
		t.Fatalf("Converge: %v", err)
	}
}

// waitForDeparture waits until the registry no longer has an installed
// generation, i.e. until the daemon has observed a runner's death through the
// ordinary path. Death observation is asynchronous (the drain goroutine sees
// the stream close), and a scan that ran before it would legitimately skip the
// still-installed socket.
func (h *discoveryHarness) waitForDeparture(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(h.boot.Registry.Snapshot()) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the daemon to observe the runner's death")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *discoveryHarness) scan(t *testing.T) {
	t.Helper()
	if err := h.boot.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

// ── the reaper predicate ────────────────────────────────────────────────────

// Mutation: any of the guards in reapStaleSocket. Each row is a distinct
// reason the daemon must keep its hands off a pathname.
func TestReapStaleSocketDeclines(t *testing.T) {
	t.Run("no lock file (runner predates the lease protocol)", func(t *testing.T) {
		dir := socketDir(t)
		ep := crashedRunnerSocket(t, dir, "a.sock")
		if err := os.Remove(socklease.LockPath(ep)); err != nil {
			t.Fatal(err)
		}
		requireDeclined(t, ep, "lease file")
	})

	t.Run("lease held by a live runner", func(t *testing.T) {
		dir := socketDir(t)
		ep := crashedRunnerSocket(t, dir, "a.sock")
		lease, err := socklease.Acquire(ep)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		requireDeclined(t, ep, "lease is held")
	})

	t.Run("outside the trusted socket directory", func(t *testing.T) {
		socketDir(t) // trusted dir is elsewhere
		other := t.TempDir()
		ep := crashedRunnerSocket(t, other, "a.sock")
		requireDeclined(t, ep, "trusted socket directory")
	})

	t.Run("not a socket", func(t *testing.T) {
		dir := socketDir(t)
		ep := filepath.Join(dir, "a.sock")
		if err := os.WriteFile(ep, []byte("not a socket"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(socklease.LockPath(ep), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		requireDeclined(t, ep, "not a socket")
	})

	t.Run("socket is live", func(t *testing.T) {
		dir := socketDir(t)
		r := startLiveRunner(t, dir, "a.sock", "184lbyqm", false)
		// A live socket with a free lock file: the lease says nothing, only
		// the probe does.
		if err := os.WriteFile(socklease.LockPath(r.ep), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		requireDeclined(t, r.ep, "live")
	})

	t.Run("pathname vanished", func(t *testing.T) {
		dir := socketDir(t)
		ep := crashedRunnerSocket(t, dir, "a.sock")
		if err := os.Remove(ep); err != nil {
			t.Fatal(err)
		}
		requireDeclined(t, ep, "not a socket")
	})
}

func requireDeclined(t *testing.T, ep, wantReason string) {
	t.Helper()
	before, existed := socklease.StatSocket(ep)
	outcome := reapStaleSocket(ep)
	if outcome.Reaped {
		t.Fatalf("reapStaleSocket(%s) removed the pathname; want declined (%s)", ep, wantReason)
	}
	if !strings.Contains(outcome.Reason, wantReason) {
		t.Errorf("reason = %q, want it to mention %q", outcome.Reason, wantReason)
	}
	if existed {
		after, stillThere := socklease.StatSocket(ep)
		if !stillThere || after != before {
			t.Error("the pathname was disturbed by a declined reap")
		}
	}
}

// Mutation: remove the lease acquisition, the probe, or the identity check —
// each is covered by a decline case above; this is the positive case.
func TestReapStaleSocketRemovesAbandonedSocketAndLockFile(t *testing.T) {
	dir := socketDir(t)
	ep := crashedRunnerSocket(t, dir, "a.sock")

	if outcome := reapStaleSocket(ep); !outcome.Reaped {
		t.Fatalf("reapStaleSocket declined an abandoned socket: %s", outcome.Reason)
	}
	if _, err := os.Lstat(ep); !os.IsNotExist(err) {
		t.Fatalf("socket pathname survived the reap: %v", err)
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); !os.IsNotExist(err) {
		t.Fatalf("lock file survived the reap: %v; the socket directory would grow without bound", err)
	}
	// And the reap is idempotent: a second attempt finds nothing to do.
	if outcome := reapStaleSocket(ep); outcome.Reaped {
		t.Fatal("second reap claimed to remove an already-removed pathname")
	}
}

// ── the two call sites, pinned independently ────────────────────────────────

// Mutation: delete the reap from the periodic scan path. The convergence path
// must not be able to mask it.
func TestScanReapsStaleSocket(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.converge(t) // nothing to converge yet: closes the barrier
	ep := crashedRunnerSocket(t, h.dir, "1lu55wqv.sock")

	h.scan(t)

	if _, err := os.Lstat(ep); !os.IsNotExist(err) {
		t.Fatalf("Scan left the stale socket behind: %v", err)
	}
	if got := h.notices.countContaining("reaped stale socket"); got != 1 {
		t.Fatalf("reap notices = %d, want exactly 1: %v", got, h.notices.all())
	}
	// A reap is a transition, not a steady state: further scans say nothing.
	for range 50 {
		h.scan(t)
	}
	if got := h.notices.countContaining("reaped stale socket"); got != 1 {
		t.Fatalf("reap notices after 50 further scans = %d, want 1", got)
	}
	if got := h.errs.count(); got != 0 {
		t.Fatalf("errors reported = %d, want 0: %v", got, h.errs.all())
	}
}

// Mutation: delete the reap from the convergence path. The scan path must not
// be able to mask it.
func TestConvergeReapsStaleSocket(t *testing.T) {
	h := newDiscoveryHarness(t)
	ep := crashedRunnerSocket(t, h.dir, "1lu55wqv.sock")

	h.converge(t)

	if _, err := os.Lstat(ep); !os.IsNotExist(err) {
		t.Fatalf("Converge left the stale socket behind: %v", err)
	}
	if got := h.notices.countContaining("reaped stale socket"); got != 1 {
		t.Fatalf("reap notices = %d, want exactly 1: %v", got, h.notices.all())
	}
}

// A socket the reaper declines to touch must still be reported — once.
//
// Mutation: report on every tick (drop the transition check), or never report
// (swallow declines).
func TestScanReportsUnreapableStaleSocketOnceOnly(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.converge(t)
	ep := crashedRunnerSocket(t, h.dir, "1u6d750s.sock")
	if err := os.Remove(socklease.LockPath(ep)); err != nil {
		t.Fatal(err) // a pre-lease runner's leftovers
	}

	for range 200 {
		h.scan(t)
	}

	if _, err := os.Lstat(ep); err != nil {
		t.Fatalf("an unleased socket was reaped: %v", err)
	}
	if got := h.errs.count(); got != 1 {
		t.Fatalf("reports for 200 identical failures = %d, want 1: %v", got, h.errs.all())
	}
	if !strings.Contains(h.errs.all()[0], "lease file") {
		t.Errorf("report does not explain the decline: %q", h.errs.all()[0])
	}
}

// ── identity-based suppression ──────────────────────────────────────────────

// Mutation: skip on pathname equality instead of socket identity.
//
// A pathname rebound by a new runner is a new socket and must be probed. Here
// the new runner claims the same session id as the installed generation, which
// makes the collision observable rather than silently ignored.
func TestScanProbesReboundPathnameAndReportsCollision(t *testing.T) {
	h := newDiscoveryHarness(t)
	first := startLiveRunner(t, h.dir, "1dvrwrho.sock", "1dvrwrho", true)
	h.converge(t)

	installed := h.boot.Registry.Snapshot()
	if len(installed) != 1 || !installed[0].Socket.Known() {
		t.Fatalf("expected one installed generation with a known socket, got %+v", installed)
	}
	before := installed[0].Socket

	// The pathname is rebound by a different process claiming the same id.
	// The installed generation keeps serving its (now unnamed) socket, so its
	// stream stays alive and its generation stays installed -- exactly what a
	// runner does between /kill and its final drain.
	// Park the original aside rather than unlinking it, so its inode stays
	// linked and the rebind necessarily lands on a different one: a filesystem
	// that recycles immediately would otherwise hand back an identity equal to
	// the installed generation's, and the scan would be right to skip it.
	parked := first.ep + ".parked"
	if err := os.Rename(first.ep, parked); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parked) })
	second := startLiveRunner(t, h.dir, "1dvrwrho.sock", "1dvrwrho", false)
	after, ok := socklease.StatSocket(second.ep)
	if !ok {
		t.Fatal("the replacement is not a socket")
	}
	if after.Same(before) {
		t.Fatalf("the replacement carries the installed generation's identity (%s); "+
			"parking the original aside should have made that impossible", after)
	}

	h.scan(t)

	if got := h.errs.countContaining("already installed"); got != 1 {
		t.Fatalf("collision reports = %d, want 1: %v", got, h.errs.all())
	}
}

// Mutation: suppress every ErrGenerationActive (the "expected race" shortcut).
//
// A *different* endpoint claiming a session id that is already installed is
// a real conflict — two processes believing they are the same session — and
// must never be silenced by the same-socket shortcut.
func TestScanReportsDistinctEndpointCollision(t *testing.T) {
	h := newDiscoveryHarness(t)
	startLiveRunner(t, h.dir, "1mw5c5n9.sock", "15w5nia8", true)
	h.converge(t)

	startLiveRunner(t, h.dir, "18wnzse2.sock", "15w5nia8", true)

	h.scan(t)
	if got := h.errs.countContaining("already installed"); got != 1 {
		t.Fatalf("collision reports = %d, want 1: %v", got, h.errs.all())
	}
	// Steady state: the collision persists but is not re-reported.
	for range 50 {
		h.scan(t)
	}
	if got := h.errs.countContaining("already installed"); got != 1 {
		t.Fatalf("collision reports after 50 further scans = %d, want 1", got)
	}
}

// Mutation: treat an unknown identity as installed (skip it).
//
// An endpoint the daemon cannot identify must keep being probed — that is the
// only way it can ever converge — but must not keep being logged.
func TestScanKeepsProbingUnidentifiableEndpointsButReportsOnce(t *testing.T) {
	socketDir(t)
	store, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const ep = "synthetic-endpoint" // not a filesystem socket: unidentifiable
	meta := sessioncoord.RunnerMeta{Registration: centralstore.RunnerRegistration{
		ID: "1o949uu4", Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1,
	}}
	runners := &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{ep: meta}, blocked: map[string]bool{}}
	errs := &recorder{}
	boot, err := newBootstrap(BootstrapConfig{ComposeMinInterval: -1, 
		Store: store, Runners: runners, Control: bootstrapControl{}, Spawner: bootstrapSpawner{},
		Reconciler: bootstrapReconciler{}, Converter: &wire.Converter{},
		Endpoints: EndpointSourceFunc(func(context.Context) ([]string, error) { return []string{ep}, nil }),
		Errors:    sessioncoord.ErrorSinkFunc(func(_ context.Context, err error) { errs.add(err.Error()) }),
		Notices:   func(context.Context, string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	if _, err := boot.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 300 {
		if err := boot.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := runners.subscribeCalls.Load(); got != 301 {
		t.Fatalf("Subscribe calls = %d, want 301: an unidentifiable endpoint must keep being probed", got)
	}
	if got := errs.count(); got != 1 {
		t.Fatalf("reports = %d, want 1 (transition only): %v", got, errs.all())
	}
}

// Mutation: report generic (neither refused nor collision) registration
// failures on every tick instead of on transition. A permanently broken
// endpoint must cost one line, not one line per tick forever.
func TestScanReportsRepeatedRegistrationFailureOnceOnly(t *testing.T) {
	store, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const ep = "broken-endpoint" // the fake transport has no meta for it
	runners := &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{}, blocked: map[string]bool{}}
	errs := &recorder{}
	boot, err := newBootstrap(BootstrapConfig{ComposeMinInterval: -1, 
		Store: store, Runners: runners, Control: bootstrapControl{}, Spawner: bootstrapSpawner{},
		Reconciler: bootstrapReconciler{}, Converter: &wire.Converter{},
		Endpoints: EndpointSourceFunc(func(context.Context) ([]string, error) { return []string{ep}, nil }),
		Errors:    sessioncoord.ErrorSinkFunc(func(_ context.Context, err error) { errs.add(err.Error()) }),
		Notices:   func(context.Context, string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	if _, err := boot.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	convergeReports := errs.count()
	if convergeReports != 1 {
		t.Fatalf("convergence reports = %d, want 1: %v", convergeReports, errs.all())
	}
	for range 200 {
		if err := boot.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// One more line than convergence produced: the periodic phase reports its
	// own first occurrence, then goes quiet.
	if got := errs.count(); got != convergeReports+1 {
		t.Fatalf("reports after 200 identical failures = %d, want %d: %v", got, convergeReports+1, errs.all())
	}
	// 201 registration probes plus 200 orphan probes: an endpoint the daemon
	// cannot identify is never suppressed, it is only reported once.
	if got := runners.metaCalls.Load(); got != 401 {
		t.Fatalf("Meta calls = %d, want 401: a broken endpoint must keep being probed", got)
	}
}

// ── diagnostic lifecycle ────────────────────────────────────────────────────

// Mutation: never clear diagnostic state on success.
//
// Without the clear, an endpoint that fails, recovers and fails again the same
// way is reported once in its life: the second incident is silently swallowed.
func TestDiagnosticsReportRecoveryAndReoccurrence(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.converge(t)

	// Incident 1: a socket that refuses and cannot be reaped.
	ep := crashedRunnerSocket(t, h.dir, "1rw58zj1.sock")
	if err := os.Remove(socklease.LockPath(ep)); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if got := h.errs.count(); got != 1 {
		t.Fatalf("after incident 1: reports = %d, want 1: %v", got, h.errs.all())
	}

	// Recovery: a live runner takes over the pathname.
	if err := os.Remove(ep); err != nil {
		t.Fatal(err)
	}
	runner := startLiveRunner(t, h.dir, "1rw58zj1.sock", "1rw58zj1", true)
	h.scan(t)
	if got := h.notices.countContaining("recovered"); got != 1 {
		t.Fatalf("recovery notices = %d, want 1: %v", got, h.notices.all())
	}

	// Incident 2: the same failure again, at a new incarnation of the
	// pathname. It must be reported, not swallowed as "unchanged".
	runner.crash(t)
	h.waitForDeparture(t)
	if err := os.Remove(socklease.LockPath(ep)); err != nil {
		t.Fatal(err)
	}
	h.scan(t)
	if got := h.errs.count(); got != 2 {
		t.Fatalf("after incident 2: reports = %d, want 2: %v", got, h.errs.all())
	}
}

// Mutation: drop diag.retain from Scan.
//
// Endpoints that are reaped or vanish never clear their own state, so without
// the sweep the map grows for the life of the daemon.
func TestDiagnosticStateDoesNotAccumulateForVanishedEndpoints(t *testing.T) {
	h := newDiscoveryHarness(t)
	h.converge(t)

	for i := range 20 {
		ep := crashedRunnerSocket(t, h.dir, fmt.Sprintf("%08d.sock", i))
		if err := os.Remove(socklease.LockPath(ep)); err != nil {
			t.Fatal(err) // unreapable, so each one is diagnosed...
		}
		h.scan(t)
		if err := os.Remove(ep); err != nil {
			t.Fatal(err) // ...and then disappears
		}
	}
	h.scan(t)

	if got := h.boot.diag.size(); got != 0 {
		t.Fatalf("retained diagnostic entries = %d, want 0", got)
	}
}

// rebindToADistinctInode replaces the socket at ep with one whose identity is
// provably different from original, and fails the test if it cannot.
//
// The obvious way -- unlink, then bind again -- is not deterministic: a
// filesystem is free to hand back the inode just freed, and GitHub's runners do,
// which made this schedule collapse into "the replacement is the original" and
// the reaper's removal of it entirely correct. The test then failed for a reason
// that had nothing to do with the reaper.
//
// So the original inode is not freed at all: it is renamed aside, which keeps it
// linked and therefore unusable, and only then is the pathname bound again. Any
// inode the kernel hands out now is necessarily a different one. The retry loop
// exists so a filesystem that still returns an equal identity accumulates
// *retained* consumers rather than looping forever on the same free inode, and
// the precondition is asserted before returning: no skip, no assumption.
func rebindToADistinctInode(t *testing.T, ep string, original socklease.Ident) socklease.Ident {
	t.Helper()

	// Everything parked here stays linked (and so unusable) until the test ends.
	var parked []string
	t.Cleanup(func() {
		for _, p := range parked {
			_ = os.Remove(p)
		}
	})

	for attempt := range 32 {
		aside := fmt.Sprintf("%s.parked-%d", ep, attempt)
		if err := os.Rename(ep, aside); err != nil {
			t.Errorf("park %s aside: %v", ep, err)
			return socklease.Ident{}
		}
		parked = append(parked, aside)

		ln, err := net.Listen("unix", ep)
		if err != nil {
			t.Errorf("listen: %v", err)
			return socklease.Ident{}
		}
		// The replacement is a *stale* socket, like the original: it refuses
		// connections, so the reaper's probe still says "refused" and the only
		// thing that differs is its identity.
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()

		replacement, ok := socklease.StatSocket(ep)
		if ok && !replacement.Same(original) {
			return replacement
		}
		// Either unidentifiable or the same inode after all. Leave this one
		// parked too, consuming whatever the kernel just handed out, and try
		// again.
	}
	t.Errorf("could not rebind %s to an inode distinct from %+v in 32 attempts; "+
		"the schedule this test asserts about cannot be expressed here", ep, original)
	return socklease.Ident{}
}

// Mutation: release with Release (history-erasing) on the reaper's
// learned-nothing declines.
//
// The lock file is the only evidence that a leftover socket belonged to a
// lease-aware generation, and that evidence is what makes it reclaimable. A
// reaper that erases it while merely inspecting turns a recoverable
// crashed-runner socket into one nothing can ever clean up or rebind: every
// later attempt sees no lock file and concludes "not provably ours".
//
// The decline driven here is the genuinely uninformative one: the pathname's
// identity changes between the probe and the removal, so the reaper knows
// nothing about whatever is there now.
func TestReaperKeepsLeaseHistoryWhenItLearnsNothing(t *testing.T) {
	dir := socketDir(t)
	ep := crashedRunnerSocket(t, dir, "1t3di8ht.sock")
	original, ok := socklease.StatSocket(ep)
	if !ok {
		t.Fatalf("%s is not a socket", ep)
	}

	// Make the pathname change identity inside the leased window, after the
	// probe has seen the socket we captured: the reaper then declines the
	// removal without having learned anything about the newcomer.
	//
	// The barrier is scoped to this endpoint. Unscoped, it fired for whichever
	// reaper reached the phase first -- a concurrent one from another test --
	// and rewrote this pathname before this test's reaper had even started.
	fired := onReapPhase(t, ep, "before-remove", func() {
		rebindToADistinctInode(t, ep, original)
	})

	outcome := reapStaleSocket(ep)
	if !fired() {
		t.Fatal("the barrier never ran")
	}
	if outcome.Reaped {
		t.Fatal("the reaper removed a pathname whose identity changed under it")
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); err != nil {
		t.Fatalf("the reaper erased lease history on an uninformative decline: %v; "+
			"the leftover is now permanently unreclaimable", err)
	}

	// And the pathname is still reclaimable: a later pass reaps it.
	if outcome := reapStaleSocket(ep); !outcome.Reaped {
		t.Fatalf("a later reap could not recover the pathname: %s", outcome.Reason)
	}
	if _, err := os.Lstat(ep); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket survived the recovering reap: %v", err)
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file survived a successful reap: %v", err)
	}
}

// The other half of the same discipline: a decline that *did* learn something
// must erase the now-false history.
func TestReaperErasesLeaseHistoryWhenItLearnsTheOccupantIsUnleased(t *testing.T) {
	dir := socketDir(t)
	// A crashed lease-aware runner's leftovers...
	ep := crashedRunnerSocket(t, dir, "1ggoslwu.sock")
	// ...whose pathname an unleased runner has since taken over.
	if err := os.Remove(ep); err != nil {
		t.Fatal(err)
	}
	r := startLiveRunner(t, dir, "1ggoslwu.sock", "1ggoslwu", false)

	outcome := reapStaleSocket(r.ep)
	if outcome.Reaped {
		t.Fatal("the reaper removed a live socket")
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease history describing a dead generation survived proof that the occupant is unleased: %v", err)
	}
	if _, ok := socklease.StatSocket(r.ep); !ok {
		t.Fatal("the live occupant's pathname was disturbed")
	}
}

// A vanished pathname's lock file is cleaned up too: that is how lock files
// left by a runner that died between creating one and binding stop
// accumulating.
func TestReaperCleansUpALockFileWhoseSocketVanished(t *testing.T) {
	dir := socketDir(t)
	ep := crashedRunnerSocket(t, dir, "16h3i8rt.sock")
	if err := os.Remove(ep); err != nil {
		t.Fatal(err)
	}

	if outcome := reapStaleSocket(ep); outcome.Reaped {
		t.Fatal("reaped a pathname that does not exist")
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an orphaned lock file was left behind: %v", err)
	}
}

// The other uninformative decline, and the one a regression is most likely to
// get wrong: the probe neither refuses nor is accepted.
//
// A socket whose mode denies the connect is the deterministic way to construct
// it; a live owner with a full accept backlog is the same verdict arriving by
// accident (EAGAIN, which classifies as a timeout), and a genuinely wedged
// owner is the case that matters. All three tell the reaper nothing about the
// occupant, so the lease history must survive -- otherwise a wedged-but-live
// runner's socket becomes permanently unreclaimable by anything: no lock file
// means no proof of a lease-aware owner, forever.
//
// Mutation: `learnedNothing = false` on every probe failure (i.e. erase the
// lock file unless the probe was refused).
func TestReaperKeepsLeaseHistoryWhenTheProbeIsAmbiguous(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: no mode denies a connect")
	}
	dir := socketDir(t)
	ep := crashedRunnerSocket(t, dir, "14n6dtux.sock")
	// Deny the connect: the probe now fails without learning anything, exactly
	// as it would against a wedged owner.
	if err := os.Chmod(ep, 0o000); err != nil {
		t.Fatal(err)
	}

	// Precondition: the probe really is ambiguous rather than refused.
	probeErr := socklease.ProbeRefused(ep, staleProbeTimeout)
	if probeErr == nil {
		t.Skip("this platform refuses the connect despite the mode; the probe is not ambiguous here")
	}
	if errors.Is(probeErr, socklease.ErrSocketLive) {
		t.Skip("the connect succeeded despite the mode")
	}

	outcome := reapStaleSocket(ep)
	if outcome.Reaped {
		t.Fatal("the reaper removed a socket it could not probe")
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); err != nil {
		t.Fatalf("the reaper erased lease history after an ambiguous probe: %v; "+
			"a wedged-but-live runner's socket would become permanently unreclaimable", err)
	}
	if _, ok := socklease.StatSocket(ep); !ok {
		t.Fatal("the socket was disturbed")
	}

	// And once the occupant is provably gone, the retained history is what lets
	// a later pass finish the job.
	if err := os.Chmod(ep, 0o600); err != nil {
		t.Fatal(err)
	}
	if outcome := reapStaleSocket(ep); !outcome.Reaped {
		t.Fatalf("a later reap could not recover the pathname: %s", outcome.Reason)
	}
	if _, err := os.Lstat(socklease.LockPath(ep)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file survived a successful reap: %v", err)
	}
}

// The window the pin exists for, on the daemon's side.
//
// The reaper has probed the leftover and found nothing listening. That verdict
// is about a *file*, and between it and the unlink the pathname can be taken
// over by a runner that holds no lease and is excluded by nothing -- the one
// population a lease cannot serialise. Pinned before the probe, the reaper holds
// the file it decided about, the pathname no longer resolves to it, and the
// removal declines. Pinned after the probe, it would hold the newcomer instead
// and unlink it alive and unprobed.
//
// Mutation: move PinSocket after probeRefused (which now requires passing a pin
// that does not exist yet, so the mutation has to pass nil -- and either way this
// test unlinks a live runner).
func TestReaperDoesNotUnlinkATakeoverRightAfterItsProbe(t *testing.T) {
	dir := socketDir(t)
	ep := crashedRunnerSocket(t, dir, "1g5qke2p.sock")

	// An unleased runner takes the pathname the instant the probe has answered.
	var replacement *net.UnixListener
	var replacementID socklease.Ident
	fired := onReapPhase(t, ep, "probed", func() {
		if err := os.Remove(ep); err != nil {
			t.Errorf("remove: %v", err)
			return
		}
		ln, err := net.Listen("unix", ep)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		ul := ln.(*net.UnixListener)
		ul.SetUnlinkOnClose(false)
		go func() {
			for {
				c, acceptErr := ul.Accept()
				if acceptErr != nil {
					return
				}
				_ = c.Close()
			}
		}()
		replacement = ul
		id, ok := socklease.StatSocket(ep)
		if !ok {
			t.Error("the replacement is not a socket")
			return
		}
		replacementID = id
	})
	t.Cleanup(func() {
		if replacement != nil {
			_ = replacement.Close()
		}
	})

	outcome := reapStaleSocket(ep)
	if !fired() {
		t.Fatal("the reaper never reached the phase just after its probe")
	}
	if outcome.Reaped {
		t.Fatal("the reaper unlinked a live runner that took the pathname after the probe")
	}

	// The replacement still owns its pathname...
	current, ok := socklease.StatSocket(ep)
	if !ok {
		t.Fatal("the live replacement's pathname was unlinked")
	}
	if current != replacementID {
		t.Fatalf("the pathname was rebound under a live runner: %s -> %s", replacementID, current)
	}
	// ...and is still reachable, which is the point of not unlinking it.
	conn, err := net.DialTimeout("unix", ep, 2*time.Second)
	if err != nil {
		t.Fatalf("the replacement is no longer reachable at its pathname: %v", err)
	}
	_ = conn.Close()
}
