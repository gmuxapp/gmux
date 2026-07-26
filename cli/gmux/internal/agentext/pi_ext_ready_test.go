package agentext

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPiExtReportsReadyWithoutAConversation is the deadlock regression for
// ADR 0027's semantic actions: the runner blocks `gmux agent prompt` until the
// agent reports `{"op":"ready"}`, so a brand-new pi session — whose conversation
// file does not exist until a turn runs — must still report ready.
//
// The stub's getSessionFile() returns undefined, which makes reportSession bail
// out entirely. If readiness were derived from (or ordered after) the session
// bind, nothing would arrive and the first prompt of every fresh session would
// time out.
//
// It also pins the ORDER: ready is the first event the extension emits on
// session_start. Delivery is serialized, so first-posted is first-delivered,
// and a readiness event that trailed a bind would be delayed by exactly the
// bind's round trip.
func TestPiExtReportsReadyWithoutAConversation(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")
	events, stop := hookEventCollector(t, sockPath)
	defer stop()

	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	driver := `
		const ext = (await import(process.argv[2])).default;
		const handlers = {};
		ext({ on: (ev, fn) => { handlers[ev] = fn; } });
		const ctx = { sessionManager: {
			getSessionFile: () => undefined,   // brand-new session: no file yet
			getSessionId: () => "id-1",
			getSessionName: () => "",
			getCwd: () => "/tmp",
		}};
		handlers.session_start({ reason: "startup" }, ctx);
		await new Promise((r) => setTimeout(r, 300));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(nodeBin, driverPath, extPath)
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sockPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver: %v\n%s", err, out)
	}

	got := events()
	if len(got) == 0 {
		t.Fatal("a fresh session with no conversation file posted no events at all: readiness is gated on the bind")
	}
	if got[0]["op"] != "ready" {
		t.Fatalf("first event = %v, want op=ready", got[0])
	}
}

// TestPiExtReportsReadyOnEveryBind: a rebind (switch/new/resume/fork) is
// positive evidence the composer is alive, and the runner treats repeats as
// idempotent, so the extension reports readiness every time rather than
// tracking "have I said this already?" state of its own.
func TestPiExtReportsReadyOnEveryBind(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")
	events, stop := hookEventCollector(t, sockPath)
	defer stop()

	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	driver := `
		const ext = (await import(process.argv[2])).default;
		const handlers = {};
		ext({ on: (ev, fn) => { handlers[ev] = fn; } });
		const mk = (file) => ({ sessionManager: {
			getSessionFile: () => file,
			getSessionId: () => "id-1",
			getSessionName: () => "named",
			getCwd: () => "/tmp",
		}});
		handlers.session_start({ reason: "startup" }, mk("/tmp/a.jsonl"));
		handlers.session_start({ reason: "resume" }, mk("/tmp/b.jsonl"));
		await new Promise((r) => setTimeout(r, 400));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(nodeBin, driverPath, extPath)
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sockPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver: %v\n%s", err, out)
	}

	ready := 0
	for _, ev := range events() {
		if ev["op"] == "ready" {
			ready++
		}
	}
	if ready != 2 {
		t.Fatalf("ready events = %d over two binds, want 2", ready)
	}
}
