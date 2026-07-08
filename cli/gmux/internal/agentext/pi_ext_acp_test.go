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

// TestPiExtForwardsToolCalls drives pi-ext.mjs with a stub `pi` that fires the
// tool-execution lifecycle (tool_execution_start → tool_execution_end) and
// asserts the extension forwards them to /acp/ingest as tool_call /
// tool_call_update, flattening the AgentToolResult content to text output.
func TestPiExtForwardsToolCalls(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")

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
		handlers.tool_execution_start({ toolCallId: "tc1", toolName: "bash", args: { cmd: "ls" } }, ctx);
		handlers.tool_execution_end({ toolCallId: "tc1", toolName: "bash", isError: false,
			result: { content: [{ type: "text", text: "file.txt" }, { type: "image", data: "x" }] } }, ctx);
		handlers.tool_execution_start({ toolCallId: "tc2", toolName: "read", args: { file: "a.txt" } }, ctx);
		handlers.tool_execution_end({ toolCallId: "tc2", toolName: "read", isError: true,
			result: { content: [{ type: "text", text: "boom" }] } }, ctx);
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
		// message_start + 2×(tool_call + tool_call_update) = 5
		if len(got) >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Collect only the tool-call ops (skip the leading message_start).
	var tools []map[string]any
	for _, ev := range got {
		if op, _ := ev["op"].(string); op == "tool_call" || op == "tool_call_update" {
			tools = append(tools, ev)
		}
	}
	if len(tools) != 4 {
		t.Fatalf("want 4 tool ops, got %d: %v", len(tools), got)
	}

	// tc1 start: tool_call with name + kind + JSON args, tagged with the message id.
	if tools[0]["op"] != "tool_call" || tools[0]["toolCallId"] != "tc1" || tools[0]["toolName"] != "bash" {
		t.Errorf("tc1 start = %v", tools[0])
	}
	if tools[0]["kind"] != "execute" {
		t.Errorf("tc1 kind = %v, want execute (bash→execute)", tools[0]["kind"])
	}
	if args, _ := tools[0]["args"].(string); args != `{"cmd":"ls"}` {
		t.Errorf("tc1 args = %q", args)
	}
	if id, _ := tools[0]["messageId"].(string); id != "m1" {
		t.Errorf("tc1 messageId = %q, want m1", id)
	}
	// tc1 end: completed, output flattened to the text block only (image dropped).
	if tools[1]["op"] != "tool_call_update" || tools[1]["status"] != "completed" || tools[1]["output"] != "file.txt" {
		t.Errorf("tc1 end = %v", tools[1])
	}
	// tc2 start: read tool maps to kind "read".
	if tools[2]["kind"] != "read" {
		t.Errorf("tc2 kind = %v, want read", tools[2]["kind"])
	}
	// tc2 end: failed status.
	if tools[3]["op"] != "tool_call_update" || tools[3]["status"] != "failed" || tools[3]["output"] != "boom" {
		t.Errorf("tc2 end = %v", tools[3])
	}
}
