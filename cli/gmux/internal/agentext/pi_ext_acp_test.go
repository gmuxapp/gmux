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
)

// TestPiExtForwardsAssistantTextDeltas drives the embedded pi-ext.mjs with a
// stub `pi` that fires the assistant message lifecycle (message_start →
// message_update text_delta ×N → message_end) and asserts the extension
// forwards them one-way to the runner's /acp/ingest channel as
// message_start / chunk / message_end (ADR 0021 tracer #1).
func TestPiExtForwardsAssistantTextDeltas(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")

	// Fake runner: capture every POST /acp/ingest body.
	var mu sync.Mutex
	var acpEvents []map[string]any
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/acp/ingest" {
			var ev map[string]any
			if err := json.NewDecoder(r.Body).Decode(&ev); err == nil {
				mu.Lock()
				acpEvents = append(acpEvents, ev)
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}

	driver := `
		const ext = (await import(process.argv[2])).default;
		const handlers = {};
		ext({ on: (ev, fn) => { handlers[ev] = fn; } });
		const ctx = {};
		handlers.message_start({ message: { role: "assistant" } }, ctx);
		handlers.message_update({ assistantMessageEvent: { type: "thinking_delta", delta: "pon" } }, ctx);
		handlers.message_update({ assistantMessageEvent: { type: "thinking_delta", delta: "der" } }, ctx);
		handlers.message_update({ assistantMessageEvent: { type: "text_delta", delta: "Hel" } }, ctx);
		handlers.message_update({ assistantMessageEvent: { type: "text_delta", delta: "lo" } }, ctx);
		// start/end/toolcall stream events must be ignored
		handlers.message_update({ assistantMessageEvent: { type: "thinking_start" } }, ctx);
		handlers.message_update({ assistantMessageEvent: { type: "toolcall_delta", delta: "nope" } }, ctx);
		handlers.message_end({ message: { role: "assistant", id: "msg-1" } }, ctx);
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

	deadline := time.Now().Add(2 * time.Second)
	var got []map[string]any
	for {
		mu.Lock()
		got = append([]map[string]any(nil), acpEvents...)
		mu.Unlock()
		if len(got) >= 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Expect: message_start, thinking_chunk "pon", thinking_chunk "der",
	// chunk "Hel", chunk "lo", message_end.
	// thinking_start / toolcall_delta must NOT appear.
	var ops []string
	var text, thinking string
	for _, ev := range got {
		op, _ := ev["op"].(string)
		ops = append(ops, op)
		d, _ := ev["delta"].(string)
		if op == "chunk" {
			text += d
		} else if op == "thinking_chunk" {
			thinking += d
		}
	}
	if len(ops) != 6 {
		t.Fatalf("want 6 acp events, got %d: %v", len(ops), ops)
	}
	if ops[0] != "message_start" || ops[5] != "message_end" {
		t.Errorf("bounds ops = %v", ops)
	}
	if thinking != "ponder" {
		t.Errorf("forwarded thinking = %q, want %q", thinking, "ponder")
	}
	if text != "Hello" {
		t.Errorf("forwarded text = %q, want %q", text, "Hello")
	}
	// pi's in-memory AssistantMessage has no id, so the extension mints a
	// per-turn counter; the same id must tag the whole message's deltas.
	startID, _ := got[0]["messageId"].(string)
	if startID != "m1" {
		t.Errorf("message_start messageId = %q, want m1", startID)
	}
	for _, ev := range got {
		if ev["op"] == "chunk" {
			if id, _ := ev["messageId"].(string); id != startID {
				t.Errorf("chunk messageId = %q, want %q", id, startID)
			}
		}
	}
}
