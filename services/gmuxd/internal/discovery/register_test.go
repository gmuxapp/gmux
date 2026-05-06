package discovery

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// metaHandler responds to GET /meta with the given session payload.
// Other paths get 404. /events is a no-op (a real subscription would
// open a long-lived SSE stream; tests exercise Register without
// caring about subscriptions, so this just returns 404 too).
func metaHandler(sess store.Session) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meta" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(sess)
	})
}

// TestRegisterReRegistrationPreservesPersistedSlug captures the
// load-bearing guarantee of ADR 0003: when a runner re-registers
// under an id already present in the store (resume; or
// daemon-restart-with-surviving-runner where the survivor was
// previously persisted), the persisted slug is preserved across the
// re-registration even when the runner's /meta payload reports a
// different (or empty) slug.
//
// The slug under test is the kind a tool adapter's attribution
// pipeline produces post-registration: the daemon refines it after
// the runner started and committed its own initial value, so the
// runner's /meta cannot speak to it on resume.
func TestRegisterReRegistrationPreservesPersistedSlug(t *testing.T) {
	srv := startUnixServer(t, metaHandler(store.Session{
		ID:    "sess-resume",
		Kind:  "shell",
		Cwd:   t.TempDir(),
		Alive: true,
		Pid:   4242,
		Slug:  "initial-from-runner",
	}))
	defer srv.cleanup()

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:    "sess-resume",
		Kind:  "shell",
		Cwd:   "/old/cwd",
		Alive: false, // dead: the resume target
		Slug:  "post-attribution-name",
	})

	if err := Register(sessions, nil, nil, srv.socketPath, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := sessions.Get("sess-resume")
	if !ok {
		t.Fatal("session sess-resume missing from store after Register")
	}
	if got.Slug != "post-attribution-name" {
		t.Errorf("Slug = %q, want %q (persisted slug must survive re-registration)", got.Slug, "post-attribution-name")
	}
	if !got.Alive {
		t.Errorf("Alive = false, want true (re-registration must flip alive)")
	}
	if got.Pid != 4242 {
		t.Errorf("Pid = %d, want 4242 (runtime fields must update from /meta)", got.Pid)
	}
}

// TestRegisterReRegistrationPreservesAttributionAndHistory pins
// the broader field-preservation contract of re-registration: a
// resumed runner reports fresh runtime state but cannot know the
// session's history (CreatedAt) or the FileMonitor-derived
// attribution (AdapterTitle / Subtitle / WorkspaceRoot / Remotes).
// Anything the runner doesn't own must carry across the seam,
// otherwise users see a re-titled session card and lose their
// project's birth time on every resume.
func TestRegisterReRegistrationPreservesAttributionAndHistory(t *testing.T) {
	// The runner reports fresh runtime state with empty values
	// for everything attribution / FileMonitor would have set.
	srv := startUnixServer(t, metaHandler(store.Session{
		ID:        "sess-resume",
		Kind:      "pi",
		Cwd:       "/work/repo",
		Alive:     true,
		Pid:       9001,
		StartedAt: "2026-05-06T16:00:00Z",
	}))
	defer srv.cleanup()

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:            "sess-resume",
		Kind:          "pi",
		Cwd:           "/work/repo",
		Alive:         false,
		Resumable:     true,
		CreatedAt:     "2026-04-01T10:00:00Z", // the original birth, not the resume's
		Slug:          "fix-the-login-bug",
		AdapterTitle:  "Fix the login bug",
		Subtitle:      "investigating session expiry",
		WorkspaceRoot: "/work/repo",
		Remotes:       map[string]string{"origin": "git@github.com:acme/web.git"},
	})

	if err := Register(sessions, nil, nil, srv.socketPath, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := sessions.Get("sess-resume")
	if !ok {
		t.Fatal("session missing after Register")
	}

	// Historical / attribution fields: must survive.
	if got.CreatedAt != "2026-04-01T10:00:00Z" {
		t.Errorf("CreatedAt = %q, want preserved (runner reports its own birth, not the session's)", got.CreatedAt)
	}
	if got.AdapterTitle != "Fix the login bug" {
		t.Errorf("AdapterTitle = %q, want preserved", got.AdapterTitle)
	}
	if got.Subtitle != "investigating session expiry" {
		t.Errorf("Subtitle = %q, want preserved", got.Subtitle)
	}
	if got.WorkspaceRoot != "/work/repo" {
		t.Errorf("WorkspaceRoot = %q, want preserved", got.WorkspaceRoot)
	}
	if got.Remotes["origin"] != "git@github.com:acme/web.git" {
		t.Errorf("Remotes[origin] = %q, want preserved", got.Remotes["origin"])
	}

	// Runtime fields: must update from the fresh /meta.
	if !got.Alive {
		t.Errorf("Alive = false, want true after re-registration")
	}
	if got.Pid != 9001 {
		t.Errorf("Pid = %d, want 9001", got.Pid)
	}
	if got.StartedAt != "2026-05-06T16:00:00Z" {
		t.Errorf("StartedAt = %q, want fresh from /meta", got.StartedAt)
	}
	if got.Resumable {
		t.Errorf("Resumable = true, want false (re-registered alive sessions are not resumable)")
	}
}

// TestRegisterFreshSessionRunsOnRegisterForShell captures the
// counterpart guarantee: a brand-new id (not present in the store)
// goes through the adapter's OnRegister hook so the initial slug
// gets derived. Shell is the only adapter that implements
// OnRegister at time of writing; using it makes this test exercise
// the path that the re-registration branch is required to skip.
func TestRegisterFreshSessionRunsOnRegisterForShell(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "myproject")
	srv := startUnixServer(t, metaHandler(store.Session{
		ID:    "sess-fresh",
		Kind:  "shell",
		Cwd:   cwd,
		Alive: true,
		// Empty slug from runner: forces the test to depend on
		// OnRegister rather than coincidentally passing because the
		// runner happened to have populated Slug.
		Slug: "",
	}))
	defer srv.cleanup()

	sessions := store.New()
	if err := Register(sessions, nil, nil, srv.socketPath, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := sessions.Get("sess-fresh")
	if !ok {
		t.Fatal("session sess-fresh missing from store after Register")
	}
	if got.Slug == "" {
		t.Error("Slug = \"\", want non-empty (Shell.OnRegister derives a slug from cwd)")
	}
}


// TestScanIgnoresMissingPathWhileSubscriptionAlive guards a race
// introduced by ptyserver.handleKill's early sockfile unlink:
// between the kill and the runner's exit event arriving over SSE,
// the path is gone but the subscription is still streaming. Phase 2
// of Scan must trust the subscription as the liveness signal in
// that window and NOT mark the session dead — doing so would call
// fileMon.NotifySessionDied which clears the file→session
// attribution that resume / restart needs to preserve under
// id-passthrough (ADR 0003).
func TestScanIgnoresMissingPathWhileSubscriptionAlive(t *testing.T) {
	// Set up a fake runner that holds /events open indefinitely so
	// the subscription stays IsActive throughout the test.
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-holdOpen:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.cleanup)

	sessions := store.New()
	sessions.Upsert(store.Session{
		ID:         "sess-graceful-kill",
		Kind:       "shell",
		Alive:      true,
		SocketPath: srv.socketPath,
	})

	subs := NewSubscriptions(sessions)
	subs.Subscribe("sess-graceful-kill", srv.socketPath)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	// Wait for the subscription's HTTP request to actually land at
	// the fake runner: IsActive flips true synchronously on
	// Subscribe (before the goroutine dials), but the race we're
	// reproducing only matters once the SSE stream is established
	// and the daemon is committed to that runner as its source of
	// truth.
	srv.waitOpen(t, 1)
	if !subs.IsActive("sess-graceful-kill") {
		t.Fatal("subscription dropped before test could exercise the race")
	}

	// Simulate ptyserver.handleKill's early unlink: path goes away
	// while subscription is still up.
	if err := os.Remove(srv.socketPath); err != nil {
		t.Fatalf("unlink sockfile: %v", err)
	}

	// Run a Scan in this race window. Without the fix, phase 2
	// classifies the session as dead and clears its attribution.
	Scan(sessions, subs, nil, nil)

	got, _ := sessions.Get("sess-graceful-kill")
	if !got.Alive {
		t.Errorf("session marked dead by Scan while subscription was still active; the SSE flow is the authoritative liveness signal during graceful kill")
	}
}
