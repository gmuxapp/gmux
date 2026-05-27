package discovery

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
			ID:    id,
			Kind:  "shell",
			Cwd:   "/home/user/proj",
			Alive: true,
			Pid:   12345,
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
		Kind:         "shell",
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

	Scan(sessions, subs, nil, nil)

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
		_ = json.NewEncoder(w).Encode(store.Session{ID: id, Kind: "shell", Alive: true})
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
		Kind:       "shell",
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

	Scan(sessions, subs, nil, nil)

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
		_ = json.NewEncoder(w).Encode(store.Session{ID: id, Kind: "shell", Alive: true, Pid: 99})
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
		Kind:       "shell",
		Alive:      true,
		SocketPath: sockPath,
	})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	// Scan must call Register exactly once during the dropped
	// window, which dials /meta once and Subscribes once.
	// Re-subscription is the load-bearing effect; the /meta call
	// is the observable side effect we can count on.
	Scan(sessions, subs, nil, nil)

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


