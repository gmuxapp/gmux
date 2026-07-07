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
	// thinking block dropped, only text surfaces
	if msgs[1].Role != "assistant" || msgs[1].Content[0].Text != "Sure!" {
		t.Errorf("msg1 = %+v", msgs[1])
	}
	if msgs[2].Content[0].Text != "plain string form" {
		t.Errorf("msg2 = %+v", msgs[2])
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
