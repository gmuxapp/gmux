package discovery

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"

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

	// Fake runner counts /meta calls. If Phase 1 skips correctly,
	// the count stays at zero across a Scan tick.
	var metaCalls int
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		metaCalls++
		_ = json.NewEncoder(w).Encode(store.Session{ID: id, Kind: "shell", Alive: true})
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

	// Forge an active subscription entry without actually opening
	// /events: Subscribe stamps active[id] synchronously, so
	// IsActive will return true even though the goroutine will
	// fail and exit shortly after (we don't care about its
	// lifetime here, only about Scan's behavior in the moment).
	subs := NewSubscriptions(sessions)
	subs.Subscribe(id, sockPath)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	Scan(sessions, subs, nil, nil)

	if metaCalls != 0 {
		t.Errorf("/meta called %d times during Scan; want 0 — Phase 1 must skip tracked-alive-subscribed sockets", metaCalls)
	}
}
