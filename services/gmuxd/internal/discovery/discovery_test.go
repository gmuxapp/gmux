package discovery

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// TestScanReregistersDeadButAliveRunnerAfterDaemonRestart pins the
// dead→alive reconciliation contract that Sweep's docstring promises:
// "callers (gmuxd at startup) Upsert them so the sidebar shows
// previously-seen sessions before any live runners register … if a
// live runner is still listening, discovery.Register will upsert it
// with Alive=true shortly after."
//
// The scenario:
//
//  1. A previous gmuxd persisted session sess-survivor and exited.
//  2. The runner stayed up; its socket is still bound and serving.
//  3. The new gmuxd loads sess-survivor via Sweep → store now has
//     Alive=false, SocketPath set, plus historical/attribution
//     fields (slug, created_at, ...) we must not lose.
//  4. The first Scan() tick must see the live socket, call Register,
//     and flip Alive=true while preserving the historical fields.
//
// Before the fix, Phase 1's skip predicate ("already tracked")
// short-circuited Register for any path the store already knew —
// regardless of alive state — so the session sat as resumable in
// the sidebar even though the runner was healthy, and clicking
// resume hit the collision-fallback in run.go (orphan duplicate
// session). The new predicate skips only when the existing entry
// is genuinely current (Alive AND IsActive), letting Sweep-loaded
// entries fall through to Register's documented re-registration
// branch.
func TestScanReregistersDeadButAliveRunnerAfterDaemonRestart(t *testing.T) {
	// Place the socket inside a dir we control and point Scan at it.
	sockDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", sockDir)

	const id = "sess-survivor"
	sockPath := filepath.Join(sockDir, id+".sock")

	const createdAt = "2026-01-02T03:04:05Z"

	// Fake runner: /meta reports the session as alive; /events returns
	// 404 so the subscription goroutine exits quickly (we don't assert
	// on SSE behavior here, only on the Scan→Register→Upsert seam).
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(store.Session{
			ID:      id,
			Adapter: "shell",
			Cwd:     "/home/user/proj",
			Alive:   true,
			Pid:     12345,
			// Empty Slug here mirrors what an adapter-less /meta would
			// return; the post-attribution slug in the store must win.
		})
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	// Seed the store as Sweep would: same id and SocketPath, Alive
	// false, with the historical / attribution fields that the new
	// daemon would not otherwise know.
	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:           id,
		Adapter:      "shell",
		Cwd:          "/home/user/proj",
		SocketPath:   sockPath,
		Alive:        false,
		Slug:         "post-attribution-name",
		CreatedAt:    createdAt,
		AdapterTitle: "Project Hub",
		Subtitle:     "main",
	})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	Scan(sessions, subs, nil)

	got, ok := sessions.Get(id)
	if !ok {
		t.Fatalf("session %s missing from store after Scan", id)
	}
	if !got.Alive {
		t.Errorf("Alive = false, want true (Scan must re-register a surviving runner)")
	}
	if got.Pid != 12345 {
		t.Errorf("Pid = %d, want 12345 (runtime fields must update from /meta)", got.Pid)
	}
	// Historical / attribution fields must survive the re-registration.
	if got.Slug != "post-attribution-name" {
		t.Errorf("Slug = %q, want %q (persisted slug must survive)", got.Slug, "post-attribution-name")
	}
	if got.CreatedAt != createdAt {
		t.Errorf("CreatedAt = %q, want %q (creation time must survive)", got.CreatedAt, createdAt)
	}
	if got.AdapterTitle != "Project Hub" {
		t.Errorf("AdapterTitle = %q, want %q (attribution must survive)", got.AdapterTitle, "Project Hub")
	}
	if got.Subtitle != "main" {
		t.Errorf("Subtitle = %q, want %q (attribution must survive)", got.Subtitle, "main")
	}
}

// TestScanSkipsTrackedAliveSubscribedSession is the negative case
// guarding against a flapping or thrashing predicate: a session
// already tracked, alive, and currently subscribed must NOT be
// re-Registered on every Scan tick. Re-Registering would
// needlessly dial /meta, churn subscriptions (Subscribe replaces
// any active entry by design), and risk overwriting in-flight
// runtime state with whatever /meta happens to report.
func TestScanSkipsTrackedAliveSubscribedSession(t *testing.T) {
	sockDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", sockDir)

	const id = "sess-already-current"
	sockPath := filepath.Join(sockDir, id+".sock")

	// Hold /events open so the subscription goroutine stays
	// connected for the duration of the test. Without this the
	// goroutine races with Scan: Subscribe stamps active[id]
	// synchronously, but a fast dial + 404 could clear the entry
	// before Scan reads IsActive, flipping the test from
	// "skip-thrash" to "reconcile-drop" and giving a misleading
	// failure when CI is slow.
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	var metaCalls atomic.Int64
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		metaCalls.Add(1)
		_ = json.NewEncoder(w).Encode(store.Session{ID: id, Adapter: "shell", Alive: true})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-hold:
		}
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:         id,
		Adapter:    "shell",
		Alive:      true,
		SocketPath: sockPath,
	})

	subs := NewSubscriptions(sessions)
	subs.Subscribe(id, sockPath)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	// Settle: give the subscription goroutine time to dial and
	// reach the held /events handler. IsActive is true the moment
	// Subscribe returns (synchronous map insert), but pinning the
	// SSE connection's lifecycle to the test's hold channel
	// removes any window where the goroutine could fail-and-exit
	// between Subscribe and Scan and silently turn this into a
	// different test.
	time.Sleep(50 * time.Millisecond)
	if !subs.IsActive(id) {
		t.Fatal("subscription dropped before Scan could read IsActive")
	}
	if n := metaCalls.Load(); n != 0 {
		t.Fatalf("unexpected /meta traffic before Scan: metaCalls=%d", n)
	}

	Scan(sessions, subs, nil)

	if n := metaCalls.Load(); n != 0 {
		t.Errorf("/meta called %d times during Scan; want 0 — Phase 1 must skip tracked-alive-subscribed sockets", n)
	}
}

// TestScanReregistersOnTransientSubscriptionDrop pins the
// self-healing path for the case where a session's SSE
// subscription dropped (network blip, runner /events handler
// returned early, daemon-side scanner read error) but the runner
// itself is still alive on its socket.
//
// runSubscription has no built-in reconnect: when the SSE stream
// ends, the goroutine clears active[id] and the store retains
// Alive=true with no consumer of runner /events. Phase 2's
// "// subscription will reconnect" comment is aspirational —
// nothing in the old code actually reconnects. The new Phase 1
// predicate inherits the reconnection role: tracked && alive &&
// !IsActive falls through to Register, which calls Subscribe and
// restores the stream.
//
// Without this behavior, a session whose subscription dropped
// silently loses live status/meta/exit events until the runner
// dies or the daemon restarts.
func TestScanReregistersOnTransientSubscriptionDrop(t *testing.T) {
	sockDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", sockDir)

	const id = "sess-blipped"
	sockPath := filepath.Join(sockDir, id+".sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	var metaCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		metaCalls.Add(1)
		_ = json.NewEncoder(w).Encode(store.Session{ID: id, Adapter: "shell", Alive: true, Pid: 99})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		// Close immediately so the daemon-side subscription drops.
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:         id,
		Adapter:    "shell",
		Alive:      true,
		SocketPath: sockPath,
	})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	// Scan must call Register exactly once during the dropped
	// window, which dials /meta once and Subscribes once.
	// Re-subscription is the load-bearing effect; the /meta call
	// is the observable side effect we can count on.
	Scan(sessions, subs, nil)

	if n := metaCalls.Load(); n != 1 {
		t.Errorf("/meta called %d times; want 1 (Phase 1 must reconcile alive-but-unsubscribed sessions via Register)", n)
	}
	got, _ := sessions.Get(id)
	if !got.Alive {
		t.Errorf("Alive = false after reconcile, want true")
	}
	if got.Pid != 99 {
		t.Errorf("Pid = %d, want 99 (re-registration must refresh runtime fields)", got.Pid)
	}
}

// serveMetaJSON starts a fake runner on a Unix socket whose /meta
// returns the given raw JSON body verbatim, so tests can exercise
// queryMeta against wire shapes a store.Session round-trip could
// never produce (e.g. pre-v2 key names).
func serveMetaJSON(t *testing.T, body string) (socketPath string) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return sockPath
}

// TestQueryMetaLegacyPreV2Keys covers queryMeta's legacy-read shim:
// long-lived pre-v2 runners survive a daemon upgrade and still report
// "kind" / "session_file" in /meta, so the new daemon must map them to
// the renamed fields. TODO(v2.1): drop alongside the shim.
func TestQueryMetaLegacyPreV2Keys(t *testing.T) {
	sock := serveMetaJSON(t, `{
		"id": "sess-old-runner",
		"kind": "pi",
		"session_file": "/home/u/.pi/agent/sessions/x/conv.jsonl",
		"alive": true
	}`)

	sess, err := queryMeta(sock)
	if err != nil {
		t.Fatalf("queryMeta: %v", err)
	}
	if sess.Adapter != "pi" {
		t.Errorf("Adapter = %q, want %q (legacy \"kind\" fallback)", sess.Adapter, "pi")
	}
	if want := "/home/u/.pi/agent/sessions/x/conv.jsonl"; sess.ConversationRef != want {
		t.Errorf("ConversationRef = %q, want %q (legacy \"session_file\" fallback)", sess.ConversationRef, want)
	}
}

// TestQueryMetaNewKeysWinOverLegacy pins the shim's precedence: the
// fallback only fills fields the new keys left empty. A payload
// carrying both (never emitted by a real runner, but the invariant
// that keeps the shim safe to leave in place) must resolve to the new
// keys' values.
func TestQueryMetaNewKeysWinOverLegacy(t *testing.T) {
	sock := serveMetaJSON(t, `{
		"id": "sess-mixed",
		"adapter": "claude",
		"kind": "pi",
		"conversation_file": "/new/conv.jsonl",
		"session_file": "/old/conv.jsonl",
		"alive": true
	}`)

	sess, err := queryMeta(sock)
	if err != nil {
		t.Fatalf("queryMeta: %v", err)
	}
	if sess.Adapter != "claude" {
		t.Errorf("Adapter = %q, want %q (new key must win)", sess.Adapter, "claude")
	}
	if sess.ConversationRef != "/new/conv.jsonl" {
		t.Errorf("ConversationRef = %q, want /new/conv.jsonl (new key must win)", sess.ConversationRef)
	}
}

// serveAliveRunner binds a fake runner at sockPath whose /meta reports
// the given session id as alive. /events is left unregistered (404) so
// the subscription goroutine exits quickly; these tests assert on the
// Scan→Register→Upsert seam, not SSE behavior.
func serveAliveRunner(t *testing.T, sockPath, id string) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(store.Session{ID: id, Adapter: "shell", Alive: true, Pid: 1})
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
}

// TestScanDiscoversRunnersInPrimaryAndLegacyDirs pins the upgrade
// contract of the XDG_RUNTIME_DIR → state-dir socket move: runners
// outlive gmuxd upgrades, so a runner that bound its socket in the old
// default ($XDG_RUNTIME_DIR/gmux/sessions) must stay discoverable —
// and attachable via its stored absolute SocketPath — alongside
// runners in the new default (StateDir()/run/sessions). Without the
// legacy scan, every pre-upgrade session would go dark on daemon
// upgrade exactly the way the logind-teardown incident stranded them.
func TestScanDiscoversRunnersInPrimaryAndLegacyDirs(t *testing.T) {
	// Route every SessionSocketDir/LegacySessionSocketDirs branch into
	// isolated temp dirs: no GMUX_SOCKET_DIR override (that disables
	// the legacy scan by design), fresh XDG dirs, and a fresh TMPDIR so
	// the per-uid temp fallback can't pick up real sockets on the host.
	t.Setenv("GMUX_SOCKET_DIR", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())

	primary := paths.SessionSocketDir()
	legacy := paths.LegacySessionSocketDirs()[0]
	for _, dir := range []string{primary, legacy} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	newSock := filepath.Join(primary, "sess-new.sock")
	oldSock := filepath.Join(legacy, "sess-old.sock")
	serveAliveRunner(t, newSock, "sess-new")
	serveAliveRunner(t, oldSock, "sess-old")

	sessions := store.New()
	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	Scan(sessions, subs, nil)

	for id, wantSock := range map[string]string{"sess-new": newSock, "sess-old": oldSock} {
		got, ok := sessions.Get(id)
		if !ok {
			t.Fatalf("session %s missing from store after Scan", id)
		}
		if !got.Alive {
			t.Errorf("%s: Alive = false, want true", id)
		}
		if got.SocketPath != wantSock {
			t.Errorf("%s: SocketPath = %q, want %q (must keep the dir it bound in)", id, got.SocketPath, wantSock)
		}
	}
}

// TestRegisterRejectsInvalidSessionID pins the fatal-registration
// seam behind the convIndex-rehydrate resume bug: a runner that
// binds a socket under an id that is not a well-formed sess-<hex>
// (e.g. a rehydrated agent session keyed by its conversation UUID)
// must be rejected with ErrInvalidSessionID, not silently accepted
// and not confused with a transient gateway error. The runner keys
// off this to exit rather than linger as an orphan.
func TestRegisterRejectsInvalidSessionID(t *testing.T) {
	sockDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", sockDir)

	// The runner reports a bare UUID as its id — exactly what a
	// convIndex-rehydrated agent session would carry.
	const badID = "019e03b3-1111-2222-3333-444455556666"
	sockPath := filepath.Join(sockDir, badID+".sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(store.Session{ID: badID, Adapter: "pi", Alive: true})
	})
	srv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	sessions := store.New()
	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	err = Register(sessions, subs, sockPath, nil)
	if !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("Register err = %v, want ErrInvalidSessionID", err)
	}
	if _, ok := sessions.Get(badID); ok {
		t.Errorf("invalid-id session was added to the store; must be rejected")
	}
}

// TestScanForcedDeathPreservesClosedTurnStatus pins the ADR 0023
// invariant (docs/adr/0023-unified-turn-model.md §4 "Turn-state-at-
// death") for the stale-socket sweep: a session whose runner vanished
// (socket gone, no live subscription) but whose turn had already
// CLOSED must keep its Status{Working:false} so a post-death `gmux
// wait` resolves "idle" — the same verdict a live wait watching the
// clean exit would return. Phase 2 previously hard-cleared Status to
// nil, which terminalReason maps to "died": the verdict became
// timing-dependent on how the death was detected.
func TestScanForcedDeathPreservesClosedTurnStatus(t *testing.T) {
	sockDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", sockDir)

	const id = "sess-closed-turn"
	// Socket path that does NOT exist: Phase 1 discovers no sockets,
	// Phase 2 stats this path, fails, and force-marks the session dead.
	sockPath := filepath.Join(sockDir, id+".sock")

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:         id,
		Adapter:    "shell",
		Alive:      true,
		SocketPath: sockPath,
		StartedAt:  "2026-01-02T03:04:05Z",
		Status:     &store.Status{Working: false},
	})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	Scan(sessions, subs, nil)

	got, ok := sessions.Get(id)
	if !ok {
		t.Fatalf("session %s missing after Scan", id)
	}
	if got.Alive {
		t.Fatalf("Alive = true, want false (stale socket must force death)")
	}
	if got.Status == nil {
		t.Fatalf("Status = nil after forced death; want preserved closed turn (ADR 0023): a cleanly-exited session reaped via stale socket must still resolve 'idle'")
	}
	if got.Status.Working {
		t.Errorf("Status.Working = true, want false (closed turn must be preserved)")
	}
}

// TestScanForcedDeathPreservesOpenTurnStatus is the mid-turn crash
// counterpart: a session that died with its turn still OPEN
// (Working:true) must keep that evidence so `gmux wait` resolves
// "died" rather than losing the open-turn state to a nil Status.
func TestScanForcedDeathPreservesOpenTurnStatus(t *testing.T) {
	sockDir := t.TempDir()
	t.Setenv("GMUX_SOCKET_DIR", sockDir)

	const id = "sess-open-turn"
	sockPath := filepath.Join(sockDir, id+".sock")

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:         id,
		Adapter:    "shell",
		Alive:      true,
		SocketPath: sockPath,
		StartedAt:  "2026-01-02T03:04:05Z",
		Status:     &store.Status{Working: true},
	})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	Scan(sessions, subs, nil)

	got, ok := sessions.Get(id)
	if !ok {
		t.Fatalf("session %s missing after Scan", id)
	}
	if got.Alive {
		t.Fatalf("Alive = true, want false (stale socket must force death)")
	}
	if got.Status == nil {
		t.Fatalf("Status = nil after forced death; want preserved open turn (ADR 0023): a mid-turn crash must keep Working=true to resolve 'died'")
	}
	if !got.Status.Working {
		t.Errorf("Status.Working = false, want true (open turn evidence must be preserved)")
	}
}
