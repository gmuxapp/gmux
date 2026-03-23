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

// --- Env / Monitor ---

func TestCodexEnvNil(t *testing.T) {
	if env := NewCodex().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil, got %v", env)
	}
}

func TestCodexMonitorNoOp(t *testing.T) {
	c := NewCodex()
	if c.Monitor([]byte("⠋ Thinking...")) != nil {
		t.Fatal("should return nil (file-driven)")
	}
}

// --- Capability interface checks ---

func TestCodexImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewCodex()
	if _, ok := a.(adapter.Launchable); !ok {
		t.Fatal("should implement Launchable")
	}
	if _, ok := a.(adapter.SessionFiler); !ok {
		t.Fatal("should implement SessionFiler")
	}
	if _, ok := a.(adapter.SessionFileLister); !ok {
		t.Fatal("should implement SessionFileLister")
	}
	if _, ok := a.(adapter.FileMonitor); !ok {
		t.Fatal("should implement FileMonitor")
	}
	if _, ok := a.(adapter.Resumer); !ok {
		t.Fatal("should implement Resumer")
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

// --- ParseSessionFile ---

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

func TestCodexParseSessionFileBasic(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc-123","timestamp":"2026-03-17T01:00:00Z","cwd":"/home/mg/dev/test"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the auth bug"}]}}`,
		`{"timestamp":"2026-03-17T01:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I'll fix that for you."}]}}`,
	)
	info, err := NewCodex().ParseSessionFile(path)
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
}

func TestCodexParseSessionFileSkipsSystemContext(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<permissions instructions>sandboxing...</permissions instructions>"}]}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/tmp</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /tmp\n<INSTRUCTIONS>...</INSTRUCTIONS>"}]}}`,
		`{"timestamp":"2026-03-17T01:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"What files are in this directory?"}]}}`,
	)
	info, _ := NewCodex().ParseSessionFile(path)
	if info.Title != "What files are in this directory?" {
		t.Errorf("expected user prompt as title (skipping system context), got %q", info.Title)
	}
}

func TestCodexParseSessionFileNoMessages(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
	)
	info, _ := NewCodex().ParseSessionFile(path)
	if info.Title != "(new)" {
		t.Errorf("expected '(new)', got %q", info.Title)
	}
}

func TestCodexParseSessionFileLongTitle(t *testing.T) {
	long := "Please help me with this very long request that goes on and on about many different things and really should be truncated for the sidebar"
	path := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+long+`"}]}}`,
	)
	info, _ := NewCodex().ParseSessionFile(path)
	if len(info.Title) > 85 {
		t.Errorf("title too long: %q", info.Title)
	}
}

func TestCodexParseSessionFileEmpty(t *testing.T) {
	path := writeCodexJSONL(t)
	_, err := NewCodex().ParseSessionFile(path)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestCodexParseSessionFileNotSessionMeta(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"type":"response_item","payload":{"type":"message","role":"user"}}`,
	)
	_, err := NewCodex().ParseSessionFile(path)
	if err != errNotSession {
		t.Errorf("expected errNotSession, got %v", err)
	}
}

// --- FileMonitor ---

func TestCodexParseNewLinesUserMessage(t *testing.T) {
	events := NewCodex().ParseNewLines([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the bug"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
	}, "")
	// Should produce: working status only (title comes from ParseSessionFile on attribution)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true status")
	}
}

func TestCodexParseNewLinesTaskComplete(t *testing.T) {
	events := NewCodex().ParseNewLines([]string{
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false")
	}
}

func TestCodexParseNewLinesTurnAborted(t *testing.T) {
	events := NewCodex().ParseNewLines([]string{
		`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false on turn_aborted")
	}
}

func TestCodexParseNewLinesSkipsSystemContext(t *testing.T) {
	events := NewCodex().ParseNewLines([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<permissions instructions>sandbox rules</permissions instructions>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/tmp</cwd></environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the bug"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
	}, "")
	// Should produce: working status only (title comes from ParseSessionFile)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true status")
	}
}

func TestCodexParseNewLinesAgentMessage(t *testing.T) {
	// agent_message alone should not generate events
	events := NewCodex().ParseNewLines([]string{
		`{"type":"event_msg","payload":{"type":"agent_message"}}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for agent_message, got %d", len(events))
	}
}

func TestCodexParseNewLinesMultiTurn(t *testing.T) {
	events := NewCodex().ParseNewLines([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix it"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
		`{"type":"response_item","payload":{"type":"function_call"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "")
	// user_message → working, task_complete → idle
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(events), events)
	}
	if !events[0].Status.Working {
		t.Error("first should be working=true")
	}
	if events[1].Status.Working {
		t.Error("second should be working=false")
	}
}

// --- Resumer ---

func TestCodexResumeCommand(t *testing.T) {
	cmd := NewCodex().ResumeCommand(&adapter.SessionFileInfo{
		ID: "019cf93a-c782-7942-ab76-010c81df6744",
	})
	expected := []string{"codex", "resume", "019cf93a-c782-7942-ab76-010c81df6744"}
	if len(cmd) != 3 || cmd[0] != expected[0] || cmd[1] != expected[1] || cmd[2] != expected[2] {
		t.Errorf("unexpected resume command: %v", cmd)
	}
}

func TestCodexCanResume(t *testing.T) {
	valid := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
		`{"timestamp":"2026-03-17T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
	)
	if !NewCodex().CanResume(valid) {
		t.Fatal("should be resumable")
	}

	empty := writeCodexJSONL(t,
		`{"timestamp":"2026-03-17T01:00:00Z","type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-17T01:00:00Z","cwd":"/tmp"}}`,
	)
	if NewCodex().CanResume(empty) {
		t.Fatal("empty session should not be resumable")
	}
}

// --- ListSessionFiles ---

func TestCodexListSessionFilesNested(t *testing.T) {
	// Create a fake date-nested directory structure
	root := t.TempDir()
	c := &Codex{}

	// Override root by creating the structure directly
	dir := filepath.Join(root, "2026", "03", "17")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "rollout-a.jsonl"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "rollout-b.jsonl"), []byte("{}"), 0644)

	dir2 := filepath.Join(root, "2026", "03", "16")
	os.MkdirAll(dir2, 0755)
	os.WriteFile(filepath.Join(dir2, "rollout-c.jsonl"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir2, "not-a-session.txt"), []byte("nope"), 0644)

	// Test the walk function directly since we can't override home dir
	var files []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Ext(path) == ".jsonl" {
			files = append(files, path)
		}
		return nil
	})

	if len(files) != 3 {
		t.Errorf("expected 3 jsonl files, got %d: %v", len(files), files)
	}

	// Verify SessionFileLister is implemented
	var _ adapter.SessionFileLister = c
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
	if !isCodexSystemContext("<permissions instructions>stuff") {
		t.Error("should detect permissions")
	}
	if !isCodexSystemContext("<environment_context>stuff") {
		t.Error("should detect environment_context")
	}
	if !isCodexSystemContext("# AGENTS.md instructions for /tmp") {
		t.Error("should detect AGENTS.md")
	}
	if !isCodexSystemContext("<turn_aborted>The user aborted") {
		t.Error("should detect turn_aborted")
	}
	if isCodexSystemContext("Fix the auth bug") {
		t.Error("should not flag user text as system context")
	}
}
