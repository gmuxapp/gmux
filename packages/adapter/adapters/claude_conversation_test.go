package adapters

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeConversationExchanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	transcript := `{"type":"user","message":{"role":"user","content":" fix it "}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"Checking"},{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"private file contents"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done exactly"}]}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"synthetic"}}
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent private"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":" follow up "},{"type":"image","source":{}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"still secret"},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{partial`
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	ex, err := NewClaude().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 2 {
		t.Fatalf("exchanges = %#v", ex)
	}
	if ex[0].User != " fix it " || ex[0].Iterations != 2 || ex[0].Terminal != "Done exactly" {
		t.Fatalf("first exchange = %#v", ex[0])
	}
	if ex[1].User != " follow up \n\n[image]" || ex[1].Iterations != 1 || ex[1].Terminal != "" {
		t.Fatalf("second exchange = %#v", ex[1])
	}

	msgs, err := NewClaude().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if msg.Text == "private file contents" || msg.Text == "secret" || msg.Text == "subagent private" {
			t.Fatalf("private content leaked: %#v", msgs)
		}
	}
}
