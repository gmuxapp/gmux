package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewAgentMessageChunkShape(t *testing.T) {
	note, err := NewAgentMessageChunk("sess1", "m1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if note.JSONRPC != "2.0" || note.Method != MethodSessionUpdate {
		t.Fatalf("bad envelope: %+v", note)
	}
	var p UpdateParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.SessionID != "sess1" {
		t.Errorf("sessionId = %q", p.SessionID)
	}
	if p.Update.SessionUpdate != UpdateAgentMessageChunk {
		t.Errorf("sessionUpdate = %q", p.Update.SessionUpdate)
	}
	if p.Update.MessageID != "m1" {
		t.Errorf("messageId = %q", p.Update.MessageID)
	}
	if p.Update.Content.Type != ContentTypeText || p.Update.Content.Text != "hello" {
		t.Errorf("content = %+v", p.Update.Content)
	}
}

func TestNewAgentThoughtChunkShape(t *testing.T) {
	note, err := NewAgentThoughtChunk("sess1", "m1", "pondering")
	if err != nil {
		t.Fatal(err)
	}
	if note.JSONRPC != "2.0" || note.Method != MethodSessionUpdate {
		t.Fatalf("bad envelope: %+v", note)
	}
	var p UpdateParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Update.SessionUpdate != UpdateAgentThoughtChunk {
		t.Errorf("sessionUpdate = %q, want %q", p.Update.SessionUpdate, UpdateAgentThoughtChunk)
	}
	if p.Update.MessageID != "m1" {
		t.Errorf("messageId = %q", p.Update.MessageID)
	}
	if p.Update.Content.Type != ContentTypeThinking || p.Update.Content.Text != "pondering" {
		t.Errorf("content = %+v", p.Update.Content)
	}
}

func TestNewToolCallShape(t *testing.T) {
	block := ToolCallBlock("t1", "bash", `{"cmd":"ls"}`)
	note, err := NewToolCall("sess1", "m1", block)
	if err != nil {
		t.Fatal(err)
	}
	if note.JSONRPC != "2.0" || note.Method != MethodSessionUpdate {
		t.Fatalf("bad envelope: %+v", note)
	}
	var p UpdateParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Update.SessionUpdate != UpdateToolCall {
		t.Errorf("sessionUpdate = %q, want %q", p.Update.SessionUpdate, UpdateToolCall)
	}
	if p.Update.MessageID != "m1" {
		t.Errorf("messageId = %q", p.Update.MessageID)
	}
	c := p.Update.Content
	if c.Type != ContentTypeToolCall || c.ToolCallID != "t1" || c.ToolName != "bash" {
		t.Errorf("content = %+v", c)
	}
	if c.Args != `{"cmd":"ls"}` || c.Status != ToolStatusInProgress {
		t.Errorf("content args/status = %+v", c)
	}
}

func TestNewToolCallUpdateShape(t *testing.T) {
	block := ContentBlock{
		Type:       ContentTypeToolCall,
		ToolCallID: "t1",
		Status:     ToolStatusCompleted,
		Output:     "file.txt",
	}
	note, err := NewToolCallUpdate("sess1", "m1", block)
	if err != nil {
		t.Fatal(err)
	}
	var p UpdateParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.Update.SessionUpdate != UpdateToolCallUpdate {
		t.Errorf("sessionUpdate = %q, want %q", p.Update.SessionUpdate, UpdateToolCallUpdate)
	}
	c := p.Update.Content
	if c.ToolCallID != "t1" || c.Status != ToolStatusCompleted || c.Output != "file.txt" {
		t.Errorf("content = %+v", c)
	}
}

func TestNewLoadShape(t *testing.T) {
	note, err := NewLoad("s", []Message{{Role: "user", Content: []ContentBlock{TextBlock("hi")}}})
	if err != nil {
		t.Fatal(err)
	}
	if note.Method != MethodSessionLoad {
		t.Fatalf("method = %q", note.Method)
	}
	var p LoadParams
	if err := json.Unmarshal(note.Params, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", p.Messages)
	}
}

func TestLoadHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	lines := []string{
		`{"type":"session","id":"x","cwd":"/tmp"}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"install cowsay"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"skip me"},{"type":"text","text":"Sure!"}]}}`,
		`{"type":"message","message":{"role":"toolResult","content":[{"type":"text","text":"dropped"}]}}`,
		`{"type":"message","message":{"role":"user","content":"plain string form"}}`,
	}
	if err := os.WriteFile(path, []byte(join(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (2 user, 1 assistant), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content[0].Text != "install cowsay" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	// thinking + text both surface, in order, with distinct block types (slice #2)
	if msgs[1].Role != "assistant" || len(msgs[1].Content) != 2 {
		t.Fatalf("msg1 = %+v", msgs[1])
	}
	if msgs[1].Content[0].Type != ContentTypeThinking || msgs[1].Content[0].Text != "skip me" {
		t.Errorf("msg1 thinking block = %+v", msgs[1].Content[0])
	}
	if msgs[1].Content[1].Type != ContentTypeText || msgs[1].Content[1].Text != "Sure!" {
		t.Errorf("msg1 text block = %+v", msgs[1].Content[1])
	}
	if msgs[2].Content[0].Text != "plain string form" {
		t.Errorf("msg2 = %+v", msgs[2])
	}
}

func TestLoadHistoryToolCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	lines := []string{
		`{"type":"message","message":{"role":"user","content":"run ls"}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"on it"},{"type":"toolCall","id":"tc1","name":"bash","arguments":{"cmd":"ls"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","toolName":"bash","content":[{"type":"text","text":"file.txt"}],"isError":false}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc2","name":"read","arguments":{}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc2","toolName":"read","content":[{"type":"text","text":"boom"}],"isError":true}}`,
	}
	if err := os.WriteFile(path, []byte(join(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant(text+toolCall), assistant(toolCall) — toolResults fold in.
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(msgs), msgs)
	}
	// First assistant: text then a completed tool call with output.
	tc := msgs[1].Content[1]
	if tc.Type != ContentTypeToolCall || tc.ToolCallID != "tc1" || tc.ToolName != "bash" {
		t.Fatalf("tc1 block = %+v", tc)
	}
	if tc.Args != `{"cmd":"ls"}` {
		t.Errorf("tc1 args = %q", tc.Args)
	}
	if tc.Status != ToolStatusCompleted || tc.Output != "file.txt" {
		t.Errorf("tc1 status/output = %+v", tc)
	}
	// Second assistant: a failed tool call.
	tc2 := msgs[2].Content[0]
	if tc2.ToolCallID != "tc2" || tc2.Status != ToolStatusFailed || tc2.Output != "boom" {
		t.Errorf("tc2 block = %+v", tc2)
	}
}

func TestLoadHistoryMissingFileIsEmpty(t *testing.T) {
	msgs, err := LoadHistory(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("want empty, got %+v", msgs)
	}
	if m, _ := LoadHistory(""); m != nil {
		t.Fatalf("empty path should be nil, got %+v", m)
	}
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
