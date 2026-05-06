package discovery

import (
	"encoding/json"
	"net/http"
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
