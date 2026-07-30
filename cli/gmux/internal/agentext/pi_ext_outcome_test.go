package agentext

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hookEventCollector runs a fake runner socket that records every
// POST /hook/event body the extension fires at it.
func hookEventCollector(t *testing.T, sockPath string) (func() []map[string]any, func()) {
	t.Helper()
	var mu sync.Mutex
	var events []map[string]any
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
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
	return func() []map[string]any {
			mu.Lock()
			defer mu.Unlock()
			return append([]map[string]any(nil), events...)
		}, func() {
			srv.Close()
			ln.Close()
		}
}

// TestPiExtNormalizeOutcome pins the pi StopReason → gmux outcome mapping
// required by ADR 0027. It runs the real embedded extension under node, so the
// table cannot drift from what ships.
//
// Only pi's one explicit abort reason may become an interruption: interruption
// is durable state that suppresses the completion notification and makes a
// synchronous wait report a stop, so every truncated or unknown terminal state
// must fall through to error rather than masquerade as an intentional stop.
func TestPiExtNormalizeOutcome(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}

	// pi's full StopReason vocabulary plus the undefined/unknown fallbacks.
	want := map[string]string{
		"stop":          "completed",
		"aborted":       "interrupted",
		"error":         "error",
		"length":        "error",
		"toolUse":       "error",
		"":              "error", // unknown value
		"__undefined__": "error", // missing stopReason entirely
	}

	driver := `
		const { normalizeOutcome } = await import(process.argv[2]);
		const out = {};
		for (const r of ["stop", "aborted", "error", "length", "toolUse", ""]) out[r] = normalizeOutcome(r);
		out["__undefined__"] = normalizeOutcome(undefined);
		process.stdout.write(JSON.stringify(out));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	out, err := exec.Command(nodeBin, driverPath, extPath).Output()
	if err != nil {
		t.Fatalf("node driver: %v (%s)", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	for reason, wantOutcome := range want {
		if got[reason] != wantOutcome {
			t.Errorf("stopReason %q → %q, want %q", reason, got[reason], wantOutcome)
		}
	}
}

// TestPiExtAgentEndOutcomes drives the extension's real agent_end +
// agent_settled handlers and asserts the posted turn-end body, so the mapping is
// pinned where it is actually consumed (the last assistant message's stopReason
// from the message list captured at the last agent_end).
func TestPiExtAgentEndOutcomes(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	for _, tc := range []struct {
		stopReason string
		want       string
	}{
		{`"stop"`, "completed"},
		{`"aborted"`, "interrupted"},
		{`"error"`, "error"},
		{`"length"`, "error"},
		{`"toolUse"`, "error"},
		{`undefined`, "error"},
	} {
		dir := t.TempDir()
		sockPath := filepath.Join(dir, "runner.sock")
		events, stop := hookEventCollector(t, sockPath)

		extPath := filepath.Join(dir, "pi-ext.mjs")
		if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
			t.Fatalf("materialize extension: %v", err)
		}
		driver := `
			const ext = (await import(process.argv[2])).default;
			const handlers = {};
			ext({ on: (ev, fn) => { handlers[ev] = fn; } });
			const ctx = { sessionManager: {
				getSessionFile: () => "/tmp/conv.jsonl",
				getSessionId: () => "id-1",
				getSessionName: () => "",
				getCwd: () => "/tmp",
			}};
			handlers.agent_start({}, ctx);
			handlers.agent_end({ messages: [
				{ role: "user", content: "go" },
				{ role: "assistant", content: "done", stopReason: ` + tc.stopReason + ` },
			] }, ctx);
			handlers.agent_settled({}, ctx);
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

		var end map[string]any
		for _, ev := range events() {
			if ev["op"] == "turn" && ev["phase"] == "end" {
				end = ev
			}
		}
		stop()
		if end == nil {
			t.Fatalf("stopReason %s: no turn end posted", tc.stopReason)
		}
		if end["outcome"] != tc.want {
			t.Errorf("stopReason %s → outcome %v, want %q", tc.stopReason, end["outcome"], tc.want)
		}
	}
}

// TestPiExtSerializesDelivery proves the property the runner's polarity-only
// turn gate depends on: the extension never issues hook request N+1 before
// request N has settled. Without it, a turn end could overtake a later turn
// start on independent fire-and-forget requests and close the wrong turn.
//
// The fake runner holds the FIRST request open while recording arrivals, so a
// parallel sender would show overlap (and out-of-order arrival) immediately.
func TestPiExtSerializesDelivery(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")

	var mu sync.Mutex
	var order []string
	inFlight, maxInFlight := 0, 0
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		first := len(order) == 0
		order = append(order, fmt.Sprint(ev["phase"], ev["op"]))
		mu.Unlock()
		if first {
			time.Sleep(250 * time.Millisecond) // hold the first request open
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer func() { srv.Close(); ln.Close() }()

	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	// Post through the extension's real sender, back to back with no awaits
	// in between — exactly how the pi event handlers call it.
	driver := `
		const { post } = await import(process.argv[2]);
		const sock = process.env.GMUX_SESSION_SOCK;
		post(sock, { op: "turn", phase: "start" });
		post(sock, { op: "turn", phase: "end", outcome: "completed" });
		await post(sock, { op: "turn", phase: "start" });
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

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Errorf("max concurrent hook requests = %d, want 1 (delivery must be serialized)", maxInFlight)
	}
	want := []string{"startturn", "endturn", "startturn"}
	if len(order) != len(want) {
		t.Fatalf("arrivals = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("arrival order = %v, want %v", order, want)
		}
	}
}

// TestPiExtDeliveryChainSurvivesUnresponsiveRunner: serialization must never
// become a permanent stall, and must not degrade into an unbounded one. A
// runner that accepts a connection and never answers has to be abandoned
// within the extension's own POST deadline (2s) so later events still get
// through — asserted as an upper bound, not merely "eventually".
func TestPiExtDeliveryChainSurvivesUnresponsiveRunner(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	// Generous headroom over the extension's 2s deadline: the assertion is
	// "bounded by the deadline", not a benchmark, so a slow CI box still
	// passes while a chain that waits on the dead request (10s) still fails.
	const bound = 6 * time.Second

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")

	delivered := make(chan string, 4)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var seen int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if atomic.AddInt32(&seen, 1) == 1 {
			select { // first request: accept and never answer
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		delivered <- fmt.Sprint(ev["phase"])
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer func() { srv.Close(); ln.Close() }()

	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	driver := `
		const { post } = await import(process.argv[2]);
		const sock = process.env.GMUX_SESSION_SOCK;
		post(sock, { op: "turn", phase: "start" });
		await post(sock, { op: "turn", phase: "end", outcome: "completed" });
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(nodeBin, driverPath, extPath)
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sockPath)

	done := make(chan error, 1)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node: %v", err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case got := <-delivered:
		if got != "end" {
			t.Fatalf("second delivery = %q, want the turn end", got)
		}
		if elapsed := time.Since(start); elapsed > bound {
			t.Errorf("queued event took %v to leave, want < %v", elapsed, bound)
		}
	case err := <-done:
		t.Fatalf("driver exited (%v) without the queued event ever leaving: the chain blocked", err)
	case <-time.After(bound):
		_ = cmd.Process.Kill()
		t.Fatalf("queued event did not leave within %v: an unresponsive runner stalled the chain", bound)
	}
	_ = cmd.Process.Kill()
	<-done
}

// TestPiExtDeliveryChainSurvivesFailedLink: a link that fails must cost
// exactly one event, not the rest of the session, and must never surface as an
// unhandled rejection (node aborts on those, which would take pi down). A
// BigInt payload cannot be JSON-serialized, so this drives the failure through
// the real sender; the driver turns any unhandled rejection into a non-zero
// exit, which fails the test.
//
// Scope note: today the sender catches its own throws and settles, so this
// exercises the "one event lost, chain intact" property. post()'s terminal
// .catch is the belt-and-braces for a future path that rejects instead.
func TestPiExtMissingSocketNotifiesWithoutCrashingNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	driver := `
		const { post } = await import(process.argv[2]);
		process.on("uncaughtException", (e) => { console.error("uncaught:", e); process.exit(3); });
		process.on("unhandledRejection", (e) => { console.error("unhandled:", e); process.exit(4); });
		await post(process.argv[3], { op: "ready" });
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	missing := filepath.Join(dir, "missing.sock")
	cmd := exec.Command(nodeBin, driverPath, extPath, missing)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver exited %v; socket failure escaped the extension\n%s", err, out)
	}
	if !strings.Contains(string(out), "gmux hook unavailable") || !strings.Contains(string(out), "ENOENT") {
		t.Fatalf("missing-socket notification = %q, want gmux warning with ENOENT", out)
	}
}

func TestPiExtDeliveryChainSurvivesFailedLink(t *testing.T) {
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
		const { post } = await import(process.argv[2]);
		const sock = process.env.GMUX_SESSION_SOCK;
		process.on("unhandledRejection", (e) => { console.error("unhandled:", e); process.exit(3); });
		post(sock, { op: "turn", phase: "start", bad: 1n });   // unserializable
		await post(sock, { op: "turn", phase: "end", outcome: "completed" });
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(nodeBin, driverPath, extPath)
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sockPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("driver exited %v (unhandled rejection or crash)\n%s", err, out)
	}

	var sawEnd bool
	for _, ev := range events() {
		if ev["phase"] == "end" {
			sawEnd = true
		}
	}
	if !sawEnd {
		t.Fatal("a failed link poisoned the chain: the following event never left")
	}
}
