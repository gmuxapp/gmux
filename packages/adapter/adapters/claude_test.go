package adapters

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- Matching ---

func TestClaudeName(t *testing.T) {
	if NewClaude().Name() != "claude" {
		t.Fatal("expected 'claude'")
	}
}

func TestClaudeMatchDirect(t *testing.T) {
	c := NewClaude()
	if !c.Match([]string{"claude"}) {
		t.Fatal("should match 'claude'")
	}
}

func TestClaudeMatchFullPath(t *testing.T) {
	c := NewClaude()
	if !c.Match([]string{"/usr/bin/claude"}) {
		t.Fatal("should match full path")
	}
	if !c.Match([]string{"/home/user/.local/bin/claude"}) {
		t.Fatal("should match ~/.local/bin path")
	}
}

func TestClaudeMatchWrapped(t *testing.T) {
	c := NewClaude()
	if !c.Match([]string{"env", "claude"}) {
		t.Fatal("should match 'env claude'")
	}
	if !c.Match([]string{"npx", "claude", "--flag"}) {
		t.Fatal("should match 'npx claude --flag'")
	}
}

func TestClaudeMatchStopsAtDoubleDash(t *testing.T) {
	if NewClaude().Match([]string{"echo", "--", "claude"}) {
		t.Fatal("should not match 'claude' after '--'")
	}
}

func TestClaudeNoMatchOther(t *testing.T) {
	c := NewClaude()
	if c.Match([]string{"pi"}) {
		t.Fatal("should not match pi")
	}
	if c.Match([]string{"claude-desktop"}) {
		t.Fatal("should not match claude-desktop")
	}
	if c.Match([]string{"claudeai"}) {
		t.Fatal("should not match claudeai")
	}
}

// --- Env / Monitor ---

func TestClaudeEnvNil(t *testing.T) {
	if env := NewClaude().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil, got %v", env)
	}
}

func TestClaudeMonitorNoOp(t *testing.T) {
	c := NewClaude()
	if c.Monitor([]byte("⠋ Thinking...")) != nil {
		t.Fatal("should return nil (file-driven, not PTY)")
	}
	if c.Monitor([]byte("some output")) != nil {
		t.Fatal("should return nil")
	}
}

// --- Capability interface checks ---

func TestClaudeImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewClaude()
	if _, ok := a.(adapter.Launchable); !ok {
		t.Fatal("should implement Launchable")
	}
	if _, ok := a.(adapter.SessionFiler); !ok {
		t.Fatal("should implement SessionFiler")
	}
	if _, ok := a.(adapter.FileMonitor); !ok {
		t.Fatal("should implement FileMonitor")
	}
	if _, ok := a.(adapter.Resumer); !ok {
		t.Fatal("should implement Resumer")
	}
}

// --- Launchers ---

func TestClaudeLaunchers(t *testing.T) {
	launchers := NewClaude().Launchers()
	if len(launchers) != 1 {
		t.Fatalf("expected 1 launcher, got %d", len(launchers))
	}
	l := launchers[0]
	if l.ID != "claude" {
		t.Errorf("expected id 'claude', got %q", l.ID)
	}
	if l.Label != "Claude Code" {
		t.Errorf("expected label 'Claude Code', got %q", l.Label)
	}
	if len(l.Command) != 1 || l.Command[0] != "claude" {
		t.Errorf("unexpected command: %v", l.Command)
	}
}

// --- SessionDir encoding ---

func TestClaudeSessionDirEncoding(t *testing.T) {
	tests := []struct {
		cwd  string
		want string
	}{
		{"/home/mg/dev/gmux", "-home-mg-dev-gmux"},
		{"/home/mg/.local/share/chezmoi", "-home-mg--local-share-chezmoi"},
		{"/tmp/test", "-tmp-test"},
		{"/home/user/my.project", "-home-user-my-project"},
	}
	c := NewClaude()
	for _, tt := range tests {
		dir := c.SessionDir(tt.cwd)
		base := filepath.Base(dir)
		if base != tt.want {
			t.Errorf("SessionDir(%q) base = %q, want %q", tt.cwd, base, tt.want)
		}
	}
}

func TestEncodeClaudeCwd(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/home/mg/dev/gmux", "-home-mg-dev-gmux"},
		{"/home/mg/.local/share/chezmoi", "-home-mg--local-share-chezmoi"},
		{"/home/mg/dev/komodo/apps/max", "-home-mg-dev-komodo-apps-max"},
	}
	for _, tt := range tests {
		got := encodeClaudeCwd(tt.in)
		if got != tt.want {
			t.Errorf("encodeClaudeCwd(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- ParseSessionFile ---

func writeClaudeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	os.WriteFile(path, []byte(content), 0644)
	return path
}

func TestClaudeParseSessionFileFirstUserMessage(t *testing.T) {
	path := writeClaudeJSONL(t,
		`{"parentUuid":null,"type":"user","sessionId":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug in login.go"}]},"uuid":"u1"}`,
		`{"parentUuid":"u1","type":"assistant","sessionId":"abc-123","timestamp":"2026-03-15T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll fix that for you."}],"stop_reason":null},"uuid":"a1"}`,
	)
	info, err := NewClaude().ParseSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "abc-123" {
		t.Errorf("expected id abc-123, got %s", info.ID)
	}
	if info.Cwd != "/tmp/test" {
		t.Errorf("expected cwd /tmp/test, got %s", info.Cwd)
	}
	if info.Title != "Fix the auth bug in login.go" {
		t.Errorf("expected first user msg as title, got %q", info.Title)
	}
	if info.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", info.MessageCount)
	}
	if info.Slug != "fix-the-auth-bug-in-login-go" {
		t.Errorf("expected slug from first user message, got %q", info.Slug)
	}
}

func TestClaudeParseSessionFileCustomTitle(t *testing.T) {
	path := writeClaudeJSONL(t,
		`{"parentUuid":null,"type":"user","sessionId":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp","message":{"role":"user","content":[{"type":"text","text":"Fix the bug"}]},"uuid":"u1"}`,
		`{"parentUuid":"u1","type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"uuid":"a1"}`,
		`{"type":"custom-title","customTitle":"  Auth refactor  ","sessionId":"abc"}`,
	)
	info, _ := NewClaude().ParseSessionFile(path)
	if info.Title != "Auth refactor" {
		t.Errorf("expected custom title, got %q", info.Title)
	}
	// Slug uses first user message (immutable), not custom-title.
	if info.Slug != "fix-the-bug" {
		t.Errorf("expected slug from first user message, got %q", info.Slug)
	}
}

func TestClaudeParseSessionFileQueueOnly(t *testing.T) {
	path := writeClaudeJSONL(t,
		`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-03-15T10:00:00Z","sessionId":"q-123"}`,
	)
	info, err := NewClaude().ParseSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "q-123" {
		t.Errorf("expected id q-123, got %s", info.ID)
	}
	if info.Title != "(new)" {
		t.Errorf("expected '(new)', got %q", info.Title)
	}
}

func TestClaudeParseSessionFileStringContent(t *testing.T) {
	path := writeClaudeJSONL(t,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp","message":{"role":"user","content":"Help me debug this"},"uuid":"u1"}`,
	)
	info, _ := NewClaude().ParseSessionFile(path)
	if info.Title != "Help me debug this" {
		t.Errorf("expected string content as title, got %q", info.Title)
	}
}

func TestClaudeParseSessionFileLongTitleTruncated(t *testing.T) {
	long := "Please help me with this very long request that goes on and on about many different things and really should be truncated for the sidebar"
	path := writeClaudeJSONL(t,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp","message":{"role":"user","content":[{"type":"text","text":"`+long+`"}]},"uuid":"u1"}`,
	)
	info, _ := NewClaude().ParseSessionFile(path)
	if len(info.Title) > 85 {
		t.Errorf("title too long: %q", info.Title)
	}
}

func TestClaudeParseSessionFileContextRefStripped(t *testing.T) {
	path := writeClaudeJSONL(t,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp","message":{"role":"user","content":[{"type":"text","text":"@file.go\n<context ref=\"file:///tmp/file.go#L1:10\">\nfunc main() {}\n</context>\nFix the bug in main()"}]},"uuid":"u1"}`,
	)
	info, _ := NewClaude().ParseSessionFile(path)
	// Context block removed, leaves @file.go reference + trailing text
	if info.Title != "@file.go Fix the bug in main()" {
		t.Errorf("expected context stripped from title, got %q", info.Title)
	}
}

func TestClaudeParseSessionFileEmpty(t *testing.T) {
	path := writeClaudeJSONL(t)
	_, err := NewClaude().ParseSessionFile(path)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestClaudeParseSessionFileNoSessionID(t *testing.T) {
	path := writeClaudeJSONL(t,
		`{"type":"unknown","data":"something"}`,
	)
	_, err := NewClaude().ParseSessionFile(path)
	if err != errNotSession {
		t.Errorf("expected errNotSession, got %v", err)
	}
}

// --- FileMonitor ---

func TestClaudeParseNewLinesCustomTitle(t *testing.T) {
	events := NewClaude().ParseNewLines([]string{
		`{"type":"custom-title","customTitle":"My session title","sessionId":"abc"}`,
	}, "")
	if len(events) != 1 || events[0].Title != "My session title" {
		t.Errorf("expected 1 title event, got %v", events)
	}
}

func TestClaudeParseNewLinesUserMessage(t *testing.T) {
	events := NewClaude().ParseNewLines([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix the bug"}]},"uuid":"u1"}`,
	}, "")
	// Should produce: working status only (title comes from ParseSessionFile on attribution)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (status), got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true status")
	}
}

func TestClaudeParseNewLinesAssistantTextOnly(t *testing.T) {
	// Text-only assistant = turn complete
	events := NewClaude().ParseNewLines([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":null},"uuid":"a1"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false status on text-only assistant")
	}
	if events[0].Unread == nil || !*events[0].Unread {
		t.Error("expected unread=true on text-only assistant (turn complete)")
	}
}

func TestClaudeParseNewLinesAssistantToolUse(t *testing.T) {
	// tool_use in content = still working, emit working=true to re-assert.
	events := NewClaude().ParseNewLines([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"ls"}}],"stop_reason":null},"uuid":"a1"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event for tool_use, got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true for tool_use (agent loop continues)")
	}
	if events[0].Unread != nil {
		t.Error("expected unread=nil for tool_use (still working)")
	}
}

func TestClaudeParseNewLinesAssistantThinkingOnly(t *testing.T) {
	// thinking-only = intermediate, no event
	events := NewClaude().ParseNewLines([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think..."}],"stop_reason":null},"uuid":"a1"}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for thinking-only, got %d", len(events))
	}
}

func TestClaudeParseNewLinesAssistantTextAndToolUse(t *testing.T) {
	// Text + tool_use = still working (tool_use takes priority)
	events := NewClaude().ParseNewLines([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I'll run a command"},{"type":"tool_use","id":"t1","name":"bash","input":{"command":"ls"}}],"stop_reason":null},"uuid":"a1"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event for text+tool_use, got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true for text+tool_use")
	}
}

func TestClaudeParseNewLinesAssistantAborted(t *testing.T) {
	// User pressed Esc — stop_reason="stop_sequence" with text content → idle.
	// Still marks unread because the response contains text the user hasn't seen.
	events := NewClaude().ParseNewLines([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I was saying..."}],"stop_reason":"stop_sequence"},"uuid":"a1"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false on stop_sequence (abort)")
	}
	if events[0].Unread == nil || !*events[0].Unread {
		t.Error("expected unread=true (response has text content)")
	}
}

func TestClaudeParseNewLinesFullTurnCycle(t *testing.T) {
	// Complete turn: user → tool_use → tool_use → end_turn
	events := NewClaude().ParseNewLines([]string{
		`{"type":"user","message":{"role":"user","content":"fix bug"},"uuid":"u1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}],"stop_reason":null},"uuid":"a1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"bash","input":{}}],"stop_reason":null},"uuid":"a2"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"},"uuid":"a3"}`,
	}, "")
	// user=working, tool_use=working, tool_use=working, end_turn=idle
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	for i := 0; i < 3; i++ {
		if events[i].Status == nil || !events[i].Status.Working {
			t.Errorf("event %d should be working=true", i)
		}
	}
	if events[3].Status == nil || events[3].Status.Working {
		t.Error("last event should be working=false (end_turn)")
	}
}

func TestClaudeParseNewLinesIgnoresNonMessageTypes(t *testing.T) {
	events := NewClaude().ParseNewLines([]string{
		`{"type":"file-history-snapshot","files":[]}`,
		`{"type":"system","subtype":"local_command","content":"output"}`,
		`{"type":"queue-operation","operation":"dequeue"}`,
		`{"type":"last-prompt","prompt":"something"}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for non-message types, got %d", len(events))
	}
}

func TestClaudeParseNewLinesSystemMessage(t *testing.T) {
	// System messages should not generate events
	events := NewClaude().ParseNewLines([]string{
		`{"type":"system","subtype":"local_command","message":"command output"}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for system, got %d", len(events))
	}
}

func TestClaudeParseNewLinesMultiTurn(t *testing.T) {
	events := NewClaude().ParseNewLines([]string{
		`{"type":"user","message":{"role":"user","content":"Fix it"},"uuid":"u1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}],"stop_reason":null},"uuid":"a1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All done."}],"stop_reason":"end_turn"},"uuid":"a2"}`,
	}, "")
	// user → working, tool_use → working, text → idle
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %v", len(events), events)
	}
	if !events[0].Status.Working {
		t.Error("first event should be working=true (user)")
	}
	if !events[1].Status.Working {
		t.Error("second event should be working=true (tool_use)")
	}
	if events[2].Status.Working {
		t.Error("third event should be working=false (end_turn)")
	}
}

// --- Resumer ---

func TestClaudeResumeCommand(t *testing.T) {
	cmd := NewClaude().ResumeCommand(&adapter.SessionFileInfo{
		ID: "abc-123-def",
	})
	if len(cmd) != 3 || cmd[0] != "claude" || cmd[1] != "--resume" || cmd[2] != "abc-123-def" {
		t.Errorf("unexpected resume command: %v", cmd)
	}
}

func TestClaudeCanResume(t *testing.T) {
	valid := writeClaudeJSONL(t,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp","message":{"role":"user","content":"hello"},"uuid":"u1"}`,
	)
	if !NewClaude().CanResume(valid) {
		t.Fatal("should be resumable")
	}

	// Queue-only file has no messages
	queueOnly := writeClaudeJSONL(t,
		`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-03-15T10:00:00Z","sessionId":"q-123"}`,
	)
	if NewClaude().CanResume(queueOnly) {
		t.Fatal("queue-only session should not be resumable")
	}
}

// --- Helpers ---

func TestCleanClaudeUserText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain text", "Fix the bug", "Fix the bug"},
		{"with context ref", "Look at this\n<context ref=\"file:///tmp/f.go#L1:5\">code</context>\nand fix it", "Look at this\n\nand fix it"},
		{"only whitespace after strip", "  ", ""},
		{"file ref with prompt", "@file.go Fix the function", "@file.go Fix the function"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanClaudeUserText(tt.in)
			if got != tt.want {
				t.Errorf("cleanClaudeUserText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractClaudeUserTextArray(t *testing.T) {
	raw := []byte(`[{"type":"text","text":"Fix the auth bug"}]`)
	got := extractClaudeUserText(raw)
	if got != "Fix the auth bug" {
		t.Errorf("expected 'Fix the auth bug', got %q", got)
	}
}

func TestExtractClaudeUserTextString(t *testing.T) {
	raw := []byte(`"Help me debug this"`)
	got := extractClaudeUserText(raw)
	if got != "Help me debug this" {
		t.Errorf("expected 'Help me debug this', got %q", got)
	}
}

func TestExtractClaudeUserTextEmpty(t *testing.T) {
	raw := []byte(`[]`)
	got := extractClaudeUserText(raw)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
