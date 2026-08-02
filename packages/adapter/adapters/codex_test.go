package adapters

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- Matching ---

func TestCodexName(t *testing.T) {
	if NewCodex().Name() != "codex" {
		t.Fatal("expected 'codex'")
	}
}

func TestCodexMatchDirect(t *testing.T) {
	c := NewCodex()
	if !c.Match([]string{"codex"}) {
		t.Fatal("should match 'codex'")
	}
}

func TestCodexMatchFullPath(t *testing.T) {
	c := NewCodex()
	if !c.Match([]string{"/usr/bin/codex"}) {
		t.Fatal("should match full path")
	}
	if !c.Match([]string{"/home/user/.local/bin/codex"}) {
		t.Fatal("should match ~/.local/bin path")
	}
}

func TestCodexMatchWrapped(t *testing.T) {
	c := NewCodex()
	if !c.Match([]string{"env", "codex"}) {
		t.Fatal("should match 'env codex'")
	}
	if !c.Match([]string{"npx", "codex", "--flag"}) {
		t.Fatal("should match 'npx codex --flag'")
	}
}

func TestCodexMatchStopsAtDoubleDash(t *testing.T) {
	if NewCodex().Match([]string{"echo", "--", "codex"}) {
		t.Fatal("should not match 'codex' after '--'")
	}
}

func TestCodexNoMatchOther(t *testing.T) {
	c := NewCodex()
	if c.Match([]string{"claude"}) {
		t.Fatal("should not match claude")
	}
	if c.Match([]string{"codex-old"}) {
		t.Fatal("should not match codex-old")
	}
}

// --- Env ---

func TestCodexEnvNil(t *testing.T) {
	if env := NewCodex().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil, got %v", env)
	}
}

// --- Capability interface checks ---

func TestCodexImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewCodex()
	if _, ok := a.(adapter.Launchable); !ok {
		t.Fatal("should implement Launchable")
	}
	if _, ok := a.(adapter.ConversationDescriber); !ok {
		t.Fatal("should implement ConversationDescriber")
	}
	if _, ok := a.(adapter.Resumer); !ok {
		t.Fatal("should implement Resumer")
	}
	// Deliberately NOT an AgentActionEncoder: interactive adapters never
	// expose gmux's semantic steer action. Raw `gmux send` remains available;
	// ACP mode will provide typed control separately.
	if _, ok := a.(adapter.AgentActionEncoder); ok {
		t.Fatal("interactive Codex must not implement AgentActionEncoder")
	}
}

// --- Launchers ---

func TestCodexLaunchers(t *testing.T) {
	launchers := NewCodex().Launchers()
	if len(launchers) != 1 {
		t.Fatalf("expected 1 launcher, got %d", len(launchers))
	}
	l := launchers[0]
	if l.ID != "codex" {
		t.Errorf("expected id 'codex', got %q", l.ID)
	}
	if l.Label != "Codex" {
		t.Errorf("expected label 'Codex', got %q", l.Label)
	}
}

// --- DescribeConversation ---

func writeCodexJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-test.jsonl")
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	os.WriteFile(path, []byte(content), 0644)
	return path
}

func TestCodexDescribeConversationBasic(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc-123","timestamp":"2026-03-17T01:00:00Z","cwd":"/home/mg/dev/test"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the auth bug"}]}}`,
		`{"timestamp":"2026-03-17T01:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I'll fix that for you."}]}}`,
	)
	info, err := NewCodex().DescribeConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "abc-123" {
		t.Errorf("expected id abc-123, got %s", info.ID)
	}
	if info.Cwd != "/home/mg/dev/test" {
		t.Errorf("expected cwd, got %s", info.Cwd)
	}
	if info.Title != "Fix the auth bug" {
		t.Errorf("expected user msg as title, got %q", info.Title)
	}
	if info.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", info.MessageCount)
	}
	if len(info.AncestorIDs) != 0 {
		t.Errorf("expected no ancestors for in-place codex resume, got %v", info.AncestorIDs)
	}
}

func TestCodexDescribeConversationSkipsSystemContext(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<permissions instructions>sandboxing...</permissions instructions>"}]}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/tmp</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /tmp\n<INSTRUCTIONS>...</INSTRUCTIONS>"}]}}`,
		`{"timestamp":"2026-03-17T01:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"What files are in this directory?"}]}}`,
	)
	info, _ := NewCodex().DescribeConversation(path)
	if info.Title != "What files are in this directory?" {
		t.Errorf("expected user prompt as title (skipping system context), got %q", info.Title)
	}
}

func TestCodexDescribeConversationNoMessages(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
	)
	info, _ := NewCodex().DescribeConversation(path)
	if info.Title != "" {
		t.Errorf("expected empty title for a session with no messages, got %q", info.Title)
	}
}

func TestCodexDescribeConversationLongTitle(t *testing.T) {
	long := "Please help me with this very long request that goes on and on about many different things and really should be truncated for the sidebar"
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+long+`"}]}}`,
	)
	info, _ := NewCodex().DescribeConversation(path)
	if len(info.Title) > 85 {
		t.Errorf("title too long: %q", info.Title)
	}
}

func TestCodexDescribeConversationEmpty(t *testing.T) {
	path := writeCodexJSONL(t)
	_, err := NewCodex().DescribeConversation(path)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestCodexDescribeConversationNotSessionMeta(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"type":"response_item","payload":{"type":"message","role":"user"}}`,
	)
	_, err := NewCodex().DescribeConversation(path)
	if err != errNotSession {
		t.Errorf("expected errNotSession, got %v", err)
	}
}

// --- Resumer ---

func TestCodexResumeCommand(t *testing.T) {
	cmd := NewCodex().ResumeCommand(&adapter.ConversationInfo{
		ID:           "019cf93a-c782-7942-ab76-010c81df6744",
		MessageCount: 1,
	})
	expected := []string{"codex", "resume", "019cf93a-c782-7942-ab76-010c81df6744"}
	if len(cmd) != 3 || cmd[0] != expected[0] || cmd[1] != expected[1] || cmd[2] != expected[2] {
		t.Errorf("unexpected resume command: %v", cmd)
	}
}

func TestCodexResumeCommandResumability(t *testing.T) {
	c := NewCodex()
	valid := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
	)
	info, err := c.DescribeConversation(valid)
	if err != nil {
		t.Fatalf("DescribeConversation: %v", err)
	}
	if cmd := c.ResumeCommand(info); len(cmd) != 3 {
		t.Fatalf("should be resumable, got %v", cmd)
	}

	empty := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
	)
	info, err = c.DescribeConversation(empty)
	if err != nil {
		t.Fatalf("DescribeConversation: %v", err)
	}
	if cmd := c.ResumeCommand(info); cmd != nil {
		t.Fatalf("empty session should not be resumable, got %v", cmd)
	}
	if cmd := c.ResumeCommand(nil); cmd != nil {
		t.Fatalf("nil info should not be resumable, got %v", cmd)
	}
}

// --- Helpers ---

func TestExtractCodexUserText(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			"plain user text",
			`[{"type":"input_text","text":"Fix the auth bug"}]`,
			"Fix the auth bug",
		},
		{
			"skips permissions",
			`[{"type":"input_text","text":"<permissions instructions>sandbox</permissions instructions>"},{"type":"input_text","text":"Fix it"}]`,
			"Fix it",
		},
		{
			"skips environment_context",
			`[{"type":"input_text","text":"<environment_context><cwd>/tmp</cwd></environment_context>"},{"type":"input_text","text":"Help me"}]`,
			"Help me",
		},
		{
			"skips AGENTS.md",
			`[{"type":"input_text","text":"# AGENTS.md instructions for /tmp\n<INSTRUCTIONS>stuff</INSTRUCTIONS>"},{"type":"input_text","text":"Do the thing"}]`,
			"Do the thing",
		},
		{
			"only system context returns empty",
			`[{"type":"input_text","text":"<permissions instructions>rules</permissions instructions>"}]`,
			"",
		},
		{
			"empty array",
			`[]`,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCodexUserText([]byte(tt.json))
			if got != tt.want {
				t.Errorf("extractCodexUserText = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsCodexSystemContext(t *testing.T) {
	for _, context := range []string{
		"<permissions instructions>stuff</permissions instructions>",
		"<environment_context>stuff</environment_context>",
		"# AGENTS.md instructions for /tmp\n<INSTRUCTIONS>stuff</INSTRUCTIONS>",
		"<turn_aborted>The user aborted</turn_aborted>",
	} {
		if !isCodexSystemContext(context) {
			t.Errorf("should detect complete context %q", context)
		}
	}
	for _, prompt := range []string{
		"Fix the auth bug",
		"<permissions please explain the deployment steps",
		"<environment_context> is an XML element; explain it",
		"# AGENTS.md is the heading I want to use",
		"<turn_aborted> can appear in documentation",
	} {
		if isCodexSystemContext(prompt) {
			t.Errorf("should retain ambiguous user prompt %q", prompt)
		}
	}
}
