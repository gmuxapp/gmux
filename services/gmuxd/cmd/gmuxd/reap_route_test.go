package main

// reap_route_test.go — a reap decision must never be executable against
// whoever happens to own the pathname by the time it is delivered, *including*
// a runner that predates the protocol.
//
// The schedule, driven end to end over real Unix sockets and the real
// production transport:
//
//	B answers Meta at pathname P as session S with incarnation NB, and is
//	classified as the redundant generation. While the daemon still holds that
//	classification, P is handed to another runner C. The reap is delivered to P.
//
// A header on /kill cannot save C: a runner that predates the protocol ignores
// unknown headers and stops. So reaping addresses a route such a runner does
// not serve at all, and C answers 404.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"nhooyr.io/websocket"
)

// protocolRunner serves what a current runner serves: an identity on every
// response and a /reap that refuses an incarnation that is not its own. With
// legacy set it serves what a *pre-protocol* runner serves -- no identity, no
// /reap route, and a /kill that obeys unconditionally.
type protocolRunner struct {
	ep          string
	incarnation string
	legacy      bool

	killed    atomic.Bool
	reaped    atomic.Bool
	killCalls atomic.Int64
	reapCalls atomic.Int64

	// afterMeta runs at the end of the /meta handler, i.e. inside the window
	// between a classification and the action taken on it.
	afterMeta func()

	ln   *net.UnixListener
	srv  *http.Server
	once sync.Once
}

func startProtocolRunner(t *testing.T, dir, name string, id centralstore.SessionID, legacy bool) *protocolRunner {
	t.Helper()
	r := &protocolRunner{
		ep:          filepath.Join(dir, name),
		incarnation: "incarnation-" + name + "-" + string(id),
		legacy:      legacy,
	}
	ln, err := net.Listen("unix", r.ep)
	if err != nil {
		t.Fatalf("listen %s: %v", r.ep, err)
	}
	r.ln = ln.(*net.UnixListener)
	// Every removal of a pathname in these tests is explicit, exactly as in
	// production: a runner's listener outlives its pathname.
	r.ln.SetUnlinkOnClose(false)

	stamp := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if !legacy {
				w.Header().Set("X-Gmux-Incarnation", r.incarnation)
			}
			next(w, req)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", stamp(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-req.Context().Done()
	}))
	mux.HandleFunc("GET /meta", stamp(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"id": string(id), "adapter": "shell", "alive": true, "pid": os.Getpid(),
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		if !legacy {
			body["incarnation"] = r.incarnation
		}
		_ = json.NewEncoder(w).Encode(body)
		if r.afterMeta != nil {
			hook := r.afterMeta
			r.afterMeta = nil
			hook()
		}
	}))
	if legacy {
		// The real pre-protocol surface: a catch-all "/" route for the
		// WebSocket terminal. POST /reap lands on the WebSocket handshake,
		// which rejects a request with no upgrade headers -- 426, not 404.
		// A ServeMux without this route would 404 instead, and the 404 is the
		// answer this design was originally reasoned about, so the fake has to
		// have the catch-all or it proves nothing about a real legacy runner.
		mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
			c, err := websocket.Accept(w, req, nil)
			if err != nil {
				return // the handshake already wrote its own status
			}
			c.Close(websocket.StatusNormalClosure, "")
		})
	}
	// Both kinds serve /kill: it is the compatibility route for an explicit
	// user-initiated stop.
	mux.HandleFunc("POST /kill", stamp(func(w http.ResponseWriter, req *http.Request) {
		r.killCalls.Add(1)
		if want := req.Header.Get("X-Gmux-Expect-Incarnation"); !legacy && want != "" && want != r.incarnation {
			http.Error(w, "mismatch", http.StatusConflict)
			return
		}
		// A pre-protocol runner ignores the header entirely and dies.
		r.killed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	if !legacy {
		mux.HandleFunc("POST /reap", stamp(func(w http.ResponseWriter, req *http.Request) {
			r.reapCalls.Add(1)
			want := req.Header.Get("X-Gmux-Expect-Incarnation")
			if want == "" {
				http.Error(w, "reap requires an expectation", http.StatusBadRequest)
				return
			}
			if want != r.incarnation {
				http.Error(w, "mismatch", http.StatusConflict)
				return
			}
			r.reaped.Store(true)
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	r.srv = &http.Server{Handler: mux}
	go func() { _ = r.srv.Serve(r.ln) }()
	t.Cleanup(func() { r.once.Do(func() { _ = r.srv.Close() }) })
	return r
}

// handOver gives this runner's pathname to a replacement, the way a runner
// that has released ownership does: unlink the pathname, leave the listener
// alive on the unnamed inode, let somebody else bind the name.
func (r *protocolRunner) handOver(t *testing.T, dir string, legacy bool) *protocolRunner {
	t.Helper()
	if err := os.Remove(r.ep); err != nil {
		t.Fatalf("remove %s: %v", r.ep, err)
	}
	return startProtocolRunner(t, dir, filepath.Base(r.ep), "1ni9rpbn", legacy)
}

// reapHarness converges one installed generation and then reaps whatever the
// caller points it at.
type reapHarness struct {
	boot     *Bootstrap
	dir      string
	reported []string
}

// reapSocketDir points the endpoint enumeration and the reaper's trust
// boundary at an isolated directory.
func reapSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", dir)
	return dir
}

func newReapHarness(t *testing.T, session centralstore.SessionID) (*reapHarness, *protocolRunner) {
	t.Helper()
	dir := reapSocketDir(t)
	store, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	installed := startProtocolRunner(t, dir, "1r6zxosb.sock", session, false)
	h := &reapHarness{dir: dir}
	boot, err := newBootstrap(BootstrapConfig{ComposeMinInterval: -1, 
		Store: store, Runners: productionRunnerClient{}, Control: productionRunnerControl{},
		Spawner: bootstrapSpawner{}, Reconciler: bootstrapReconciler{}, Converter: &wire.Converter{},
		Endpoints: EndpointSourceFunc(func(context.Context) ([]string, error) {
			return []string{installed.ep}, nil
		}),
		Errors: sessioncoord.ErrorSinkFunc(func(_ context.Context, err error) { h.reported = append(h.reported, err.Error()) }),
		Clock:  func() centralstore.UnixMillis { return 100 },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(boot.Close)
	if _, err := boot.Converge(context.Background()); err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if snap := boot.Registry.Snapshot(); len(snap) != 1 {
		t.Fatalf("expected one installed generation, got %+v", snap)
	}
	h.boot = boot
	return h, installed
}

// The baseline: the runner the decision was actually about kills itself, over
// the conditional route, and is never addressed on /kill.
func TestReapKillsExactlyTheClassifiedRunner(t *testing.T) {
	const session = centralstore.SessionID("172idosy")
	h, _ := newReapHarness(t, session)
	loser := startProtocolRunner(t, h.dir, "1si1fsnc.sock", session, false)

	reaped, err := h.boot.Coordinator.ReapOrphans(context.Background(), []string{loser.ep})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != loser.ep {
		t.Fatalf("reaped = %v, want [%s]", reaped, loser.ep)
	}
	if !loser.reaped.Load() {
		t.Fatal("the classified runner was not reaped")
	}
	if got := loser.killCalls.Load(); got != 0 {
		t.Fatalf("the classified runner saw %d /kill requests; reaping must use the conditional route", got)
	}
}

// Mutation: point ReapOrphans back at RunnerControl.Terminate (the /kill
// route). The pre-protocol replacement then receives the verdict passed on B
// and dies.
func TestReapDoesNotKillAPreProtocolReplacement(t *testing.T) {
	const session = centralstore.SessionID("172idosy")
	h, _ := newReapHarness(t, session)
	loser := startProtocolRunner(t, h.dir, "1si1fsnc.sock", session, false)

	// The pathname changes hands inside the window between the classification
	// and the action taken on it.
	var replacement *protocolRunner
	loser.afterMeta = func() { replacement = loser.handOver(t, h.dir, true) }

	reaped, err := h.boot.Coordinator.ReapOrphans(context.Background(), []string{loser.ep})
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if replacement == nil {
		t.Fatal("the hand-over never ran")
	}

	if replacement.killed.Load() {
		t.Fatal("a pre-protocol replacement was killed by a verdict passed on another process")
	}
	if got := replacement.killCalls.Load(); got != 0 {
		t.Fatalf("the pre-protocol replacement received %d /kill requests; it must never be addressed there", got)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want nothing: the classified runner was already gone", reaped)
	}
	// It still owns its pathname and is still reachable.
	if _, err := os.Stat(replacement.ep); err != nil {
		t.Fatalf("the replacement's pathname was disturbed: %v", err)
	}
	conn, err := net.DialTimeout("unix", replacement.ep, 2*time.Second)
	if err != nil {
		t.Fatalf("the replacement is no longer reachable: %v", err)
	}
	_ = conn.Close()
	// A decline is not an error: it must not be reported as one.
	if len(h.reported) != 0 {
		t.Fatalf("declining to reap a pre-protocol occupant was reported as an error: %v", h.reported)
	}
}

// The same schedule with a protocol-aware replacement: it refuses (409) rather
// than dying, which is the case the incarnation header already covered.
func TestReapDoesNotKillAProtocolAwareReplacement(t *testing.T) {
	const session = centralstore.SessionID("172idosy")
	h, _ := newReapHarness(t, session)
	loser := startProtocolRunner(t, h.dir, "1si1fsnc.sock", session, false)

	var replacement *protocolRunner
	loser.afterMeta = func() { replacement = loser.handOver(t, h.dir, false) }

	if _, err := h.boot.Coordinator.ReapOrphans(context.Background(), []string{loser.ep}); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if replacement == nil {
		t.Fatal("the hand-over never ran")
	}
	if replacement.reaped.Load() || replacement.killed.Load() {
		t.Fatal("a replacement obeyed a verdict passed on another process")
	}
	if got := replacement.reapCalls.Load(); got != 1 {
		t.Fatalf("the replacement saw %d reap requests, want 1 (and it must have refused)", got)
	}
	if got := replacement.killCalls.Load(); got != 0 {
		t.Fatalf("the replacement was addressed on /kill %d times", got)
	}
}

// An explicit user-initiated stop still uses the compatibility route, because
// that is the only thing a pre-protocol runner understands.
func TestExplicitStopUsesTheCompatibilityRoute(t *testing.T) {
	dir := reapSocketDir(t)
	runner := startProtocolRunner(t, dir, "1whzt623.sock", "1whzt623", false)

	if err := (productionRunnerControl{}).Terminate(context.Background(), runner.ep, runner.incarnation); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !runner.killed.Load() {
		t.Fatal("an explicit stop did not reach the runner")
	}
	if got := runner.reapCalls.Load(); got != 0 {
		t.Fatalf("an explicit stop used the conditional route (%d calls)", got)
	}
	// And an unnamed stop -- the pre-protocol client's shape -- still works.
	legacy := startProtocolRunner(t, dir, "1l2nqzns.sock", "1l2nqzns", true)
	if err := (productionRunnerControl{}).Terminate(context.Background(), legacy.ep, ""); err != nil {
		t.Fatalf("unnamed Terminate: %v", err)
	}
	if !legacy.killed.Load() {
		t.Fatal("an unnamed stop did not reach a pre-protocol runner")
	}
}

// The control adapter must translate "this runner has no such route" into the
// coordinator's own sentinel, or the reap loop cannot tell a decline from a
// failure.
func TestProductionReapTranslatesAnUnsupportedRoute(t *testing.T) {
	dir := reapSocketDir(t)
	legacy := startProtocolRunner(t, dir, "1u6d750s.sock", "1u6d750s", true)

	err := (productionRunnerControl{}).Reap(context.Background(), legacy.ep, "incarnation-of-somebody")
	if !errors.Is(err, sessioncoord.ErrReapUnsupported) {
		t.Fatalf("Reap against a pre-protocol runner = %v, want ErrReapUnsupported", err)
	}
	if legacy.killed.Load() || legacy.killCalls.Load() != 0 {
		t.Fatal("a pre-protocol runner was touched by a reap attempt")
	}
}

// A pre-protocol occupant that cannot be reaped is a standing condition, not an
// incident: sweeping past it must cost nothing, forever.
//
// What this pins is the *first* guard on that path: an occupant that reports no
// incarnation is unidentifiable, so ReapOrphans skips it before any transport
// call at all, silently, on every sweep. (Mutation: reap candidates with an
// empty incarnation.) It deliberately does not pin the 426 path -- a real
// legacy runner never gets that far, because it has no incarnation to be
// classified by; that path is reached only when a *modern* runner was
// classified and a legacy one took its pathname over, which is
// TestReapDoesNotKillAPreProtocolReplacement. The decline there is stateless,
// so silence on one sweep is silence on all of them.
func TestRepeatedSweepsPastAPreProtocolOccupantAreSilent(t *testing.T) {
	const session = centralstore.SessionID("172idosy")
	h, _ := newReapHarness(t, session)
	// A pre-protocol runner claiming the installed session's id: a permanent
	// reap candidate that can never be reaped.
	legacy := startProtocolRunner(t, h.dir, "1grjqma6.sock", session, true)

	for range 50 {
		if _, err := h.boot.Coordinator.ReapOrphans(context.Background(), []string{legacy.ep}); err != nil {
			t.Fatalf("ReapOrphans: %v", err)
		}
	}
	if len(h.reported) != 0 {
		t.Fatalf("50 sweeps past an unreapable occupant produced %d reports: %v", len(h.reported), h.reported)
	}
	if legacy.killed.Load() || legacy.killCalls.Load() != 0 {
		t.Fatal("the pre-protocol occupant was touched")
	}
	if _, err := os.Stat(legacy.ep); err != nil {
		t.Fatalf("its pathname was disturbed: %v", err)
	}
}
