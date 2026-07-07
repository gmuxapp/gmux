package agentext

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// TestPiExtSessionEventCarriesSlug is a regression test for session URLs
// falling back to UUIDs: the pi extension must report an explicit slug
// source (the resolved title) in its "session" hook events, because pi's
// session id is a UUID and the runner's fallback is Slugify(id).
//
// It drives the embedded pi-ext.mjs with a stub `pi` object under real node,
// and captures what the extension POSTs to the runner socket.
func TestPiExtSessionEventCarriesSlug(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")

	// Fake runner: capture every POST /hook/event body.
	var mu sync.Mutex
	var events []map[string]any
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// Write the embedded source directly rather than via Path(): its
	// sync.Once result may point into another test's (since removed)
	// temp XDG_CACHE_HOME.
	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}

	// Stub pi: register handlers, fire session_start with a named session,
	// then a rename, and give fire-and-forget posts time to flush.
	driver := `
		const ext = (await import(process.argv[2])).default;
		const handlers = {};
		ext({ on: (ev, fn) => { handlers[ev] = fn; } });
		let name = "";
		const ctx = { sessionManager: {
			getSessionFile: () => "/tmp/conv.jsonl",
			getSessionId: () => "019f2c63-2149-7b6d-865a-4ddc2af6b684",
			getSessionName: () => name,
			getCwd: () => "/tmp",
		}};
		handlers.session_start({ reason: "startup" }, ctx);
		handlers.agent_end({ messages: [{ role: "user", content: "Fix the login bug" }] }, ctx);
		name = "Renamed Session";
		handlers.session_info_changed({}, ctx);
		await new Promise((r) => setTimeout(r, 300));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(nodeBin, driverPath, extPath)
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver: %v\n%s", err, out)
	}

	// Collect session events (posts are async; wait briefly for stragglers).
	deadline := time.Now().Add(2 * time.Second)
	var sessions []map[string]any
	for {
		mu.Lock()
		sessions = sessions[:0]
		for _, ev := range events {
			if ev["op"] == "session" {
				sessions = append(sessions, ev)
			}
		}
		n := len(sessions)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(sessions) < 2 {
		t.Fatalf("expected >=2 session events, got %d (%v)", len(sessions), events)
	}

	// The runner slugifies whatever the hook sends, so pin behavior, not the
	// wire encoding: every titled bind must carry a slug source that slugifies
	// to the title's slug — never left to the UUID id fallback. The pre-title
	// bind (session_start before any message) legitimately has no slug source.
	foundTitled := false
	for _, ev := range sessions {
		name, _ := ev["name"].(string)
		slug, _ := ev["slug"].(string)
		if name == "" {
			if slug != "" {
				t.Errorf("pre-title bind: want no slug source, got %q", slug)
			}
			continue
		}
		foundTitled = true
		if got, want := adapter.Slugify(slug), adapter.Slugify(name); got == "" || got != want {
			t.Errorf("session event with name %q: slug source %q slugifies to %q, want %q", name, slug, got, want)
		}
	}
	if !foundTitled {
		t.Fatalf("no titled session event observed: %v", sessions)
	}
}
