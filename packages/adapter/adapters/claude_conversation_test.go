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

func TestClaudeCurrentSplitResponsesGroupByMessageID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "split.jsonl")
	// Claude Code 2.1.220 persists one record per content block. Records from
	// one completed API response share message.id even when bookkeeping records
	// interleave them.
	transcript := `{"type":"user","message":{"role":"user","content":"go"}}
{"uuid":"a1","type":"assistant","message":{"id":"msg-a","role":"assistant","content":[{"type":"thinking","thinking":"secret"}]}}
{"type":"progress","message":{"role":"assistant","content":"ignored bookkeeping"}}
{"uuid":"a2","type":"assistant","message":{"id":"msg-a","role":"assistant","content":[{"type":"text","text":"Checking"}]}}
{"uuid":"a3","type":"assistant","message":{"id":"msg-a","role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}
{"uuid":"b1","type":"assistant","message":{"id":"msg-b","role":"assistant","content":[{"type":"text","text":"Answer prose"}]}}
{"uuid":"b2","type":"assistant","message":{"id":"msg-b","role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	ex, err := NewClaude().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Iterations != 2 || ex[0].Terminal != "Answer prose" {
		t.Fatalf("split exchange = %#v", ex)
	}

	messages, err := NewClaude().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[1].Text != "Checking\n\n[tool] Read {\"file_path\":\"a.go\"}" ||
		messages[2].Text != "Answer prose\n\n[tool] Bash {\"command\":\"go test ./...\"}" || messages[2].Prose != "Answer prose" {
		t.Fatalf("split messages = %#v", messages)
	}
}

func TestClaudeMissingMessageIDsRemainLegacyResponseBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	transcript := `{"type":"user","message":{"role":"user","content":"go"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"tool_use","name":"Read","input":{}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}`
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := NewClaude().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Iterations != 2 || ex[0].Terminal != "second" {
		t.Fatalf("legacy exchange = %#v", ex)
	}
}

func TestClaudeInjectedCommandRegistryRequiresProvenance(t *testing.T) {
	command := "<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "command only"},
		{name: "stdout", output: "<local-command-stdout>bookkeeping output</local-command-stdout>"},
		{name: "stderr", output: "<local-command-stderr>bookkeeping error</local-command-stderr>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "commands.jsonl")
			transcript := `{"uuid":"user","type":"user","message":{"role":"user","content":"real prompt"}}
{"uuid":"assistant","parentUuid":"user","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}
{"uuid":"caveat","parentUuid":"assistant","type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>generated caveat</local-command-caveat>"}}
` + `{"uuid":"command","parentUuid":"caveat","type":"user","isMeta":false,"message":{"role":"user","content":` + quoteJSON(command) + `}}`
			if tc.output != "" {
				transcript += "\n" + `{"uuid":"output","parentUuid":"command","type":"user","isMeta":false,"message":{"role":"user","content":` + quoteJSON(tc.output) + `}}`
			}
			if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			ex, err := NewClaude().RenderConversationExchanges(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(ex) != 1 || ex[0].User != "real prompt" || ex[0].Terminal != "answer" {
				t.Fatalf("proven injected command leaked: %#v", ex)
			}
			messages, err := NewClaude().RenderConversation(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 2 {
				t.Fatalf("proven injected command leaked into conversation: %#v", messages)
			}
		})
	}
}

func TestClaudeAmbiguousCommandShapesRemainUserPrompts(t *testing.T) {
	prompts := []string{
		"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>",
		"<local-command-stdout>user-owned example</local-command-stdout>",
		"<local-command-stderr>user-owned example</local-command-stderr>",
		"<local-command-caveat>user-owned example</local-command-caveat>",
		"<bash-input>echo user-owned</bash-input>",
		"<command-name>explain this element</command-name>",
		"<local-command-stdout>unterminated example",
		"prefix <local-command-stdout>example</local-command-stdout>",
	}
	for _, prompt := range prompts {
		t.Run(prompt, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ambiguous.jsonl")
			transcript := `{"uuid":"prompt","type":"user","message":{"role":"user","content":` + quoteJSON(prompt) + `}}
{"uuid":"answer","parentUuid":"prompt","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`
			if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			ex, err := NewClaude().RenderConversationExchanges(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(ex) != 1 || ex[0].User != prompt || ex[0].Terminal != "answer" {
				t.Fatalf("legitimate prompt erased: %#v", ex)
			}
			messages, err := NewClaude().RenderConversation(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 2 || messages[0].Text != prompt {
				t.Fatalf("legitimate prompt erased from conversation: %#v", messages)
			}
		})
	}
}

func TestClaudeConversationUsesActiveParentBranch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "branched.jsonl")
	transcript := `{"uuid":"u1","parentUuid":null,"type":"user","message":{"role":"user","content":"root"}}
{"uuid":"a-old","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}
{"uuid":"u-old","parentUuid":"a-old","type":"user","message":{"role":"user","content":"abandoned private text"}}
{"uuid":"a-new","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"active"}]}}
{"uuid":"u-new","parentUuid":"a-new","type":"user","message":{"role":"user","content":"active followup"}}
{"uuid":"a-end","parentUuid":"u-new","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := NewClaude().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 2 || ex[0].User != "root" || ex[0].Terminal != "active" || ex[1].User != "active followup" || ex[1].Terminal != "done" {
		t.Fatalf("active exchanges = %#v", ex)
	}
}

func TestClaudeConversationIgnoresTrailingHiddenLeaves(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
	}{
		{"sidechain", `{"uuid":"side","parentUuid":"u1","type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"private sidechain"}]}}`},
		{"meta", `{"uuid":"meta","parentUuid":"u1","type":"user","isMeta":true,"message":{"role":"user","content":"hidden metadata"}}`},
		{"compact summary", `{"uuid":"compact","parentUuid":"u1","type":"user","isCompactSummary":true,"message":{"role":"user","content":"hidden compact summary"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hidden-tail.jsonl")
			transcript := `{"uuid":"u1","parentUuid":null,"type":"user","message":{"role":"user","content":"root"}}
{"uuid":"main","parentUuid":"u1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"main answer"}]}}
` + tc.tail
			if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
			ex, err := NewClaude().RenderConversationExchanges(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(ex) != 1 || ex[0].Iterations != 1 || ex[0].Terminal != "main answer" {
				t.Fatalf("trailing hidden leaf erased main tail: %#v", ex)
			}
		})
	}
}

func TestClaudeConversationRetainsHiddenTraversalAncestors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidden-ancestor.jsonl")
	transcript := `{"uuid":"u1","parentUuid":null,"type":"user","message":{"role":"user","content":"root"}}
{"uuid":"hidden","parentUuid":"u1","type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"private"}]}}
{"uuid":"main","parentUuid":"hidden","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"visible continuation"}]}}`
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := NewClaude().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Iterations != 1 || ex[0].Terminal != "visible continuation" {
		t.Fatalf("hidden ancestor broke traversal: %#v", ex)
	}
}

func TestClaudeImageOnlyUserIsExchangeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.jsonl")
	transcript := `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"secret"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"seen"}]}}`
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := NewClaude().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].User != "[image]" || ex[0].Terminal != "seen" {
		t.Fatalf("image exchange = %#v", ex)
	}
}
