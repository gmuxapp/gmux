package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- Matching ---

func TestPiName(t *testing.T) {
	if NewPi().Name() != "pi" {
		t.Fatal("expected 'pi'")
	}
}

func TestPiMatchDirect(t *testing.T) {
	p := NewPi()
	if !p.Match([]string{"pi"}) {
		t.Fatal("should match 'pi'")
	}
	if !p.Match([]string{"pi-coding-agent"}) {
		t.Fatal("should match 'pi-coding-agent'")
	}
}

func TestPiMatchWrapped(t *testing.T) {
	p := NewPi()
	if !p.Match([]string{"npx", "pi"}) {
		t.Fatal("should match 'npx pi'")
	}
	if !p.Match([]string{"env", "pi", "--flag"}) {
		t.Fatal("should match 'env pi --flag'")
	}
	if !p.Match([]string{"/home/user/.local/bin/pi"}) {
		t.Fatal("should match full path")
	}
}

func TestPiMatchStopsAtDoubleDash(t *testing.T) {
	if NewPi().Match([]string{"echo", "--", "pi"}) {
		t.Fatal("should not match 'pi' after '--'")
	}
}

func TestPiNoMatchOther(t *testing.T) {
	p := NewPi()
	if p.Match([]string{"pytest"}) {
		t.Fatal("should not match pytest")
	}
	if p.Match([]string{"pipeline"}) {
		t.Fatal("should not match 'pipeline'")
	}
}

// --- Env / Monitor ---

func TestPiEnvNil(t *testing.T) {
	if env := NewPi().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil, got %v", env)
	}
}

func TestPiDiscover(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: depends on pi being installed")
	}
	// LookPath-based: result depends on the test machine.
	_ = NewPi().Discover()
}

func TestPiMonitorNoOp(t *testing.T) {
	// Pi Monitor is a no-op — status is driven by FileMonitor.
	pi := NewPi()
	if pi.Monitor([]byte("⠋ Working...")) != nil {
		t.Fatal("should return nil (file-driven, not PTY)")
	}
	if pi.Monitor([]byte("some output")) != nil {
		t.Fatal("should return nil")
	}
}

// --- Capability interface checks ---

func TestPiImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewPi()
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

// --- SessionFiler ---

func writeTempJSONL(t *testing.T, lines ...string) string {
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

func TestParseSessionFileFirstUserMessage(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"model_change","id":"m1","timestamp":"2026-03-15T10:00:00Z"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug in login.go"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-03-15T10:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"I'll fix that for you."}]}}`,
	)
	info, err := NewPi().ParseSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "abc-123" {
		t.Errorf("expected id abc-123, got %s", info.ID)
	}
	if info.Title != "Fix the auth bug in login.go" {
		t.Errorf("expected first user msg as title, got %q", info.Title)
	}
	if info.MessageCount != 2 {
		t.Errorf("expected 2 messages, got %d", info.MessageCount)
	}
	if info.Slug != "fix-the-auth-bug-in-login-go" {
		t.Errorf("expected slug from title, got %q", info.Slug)
	}
}

func TestParseSessionFileNameOverrides(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug"}]}}`,
		`{"type":"session_info","name":"  Auth refactor  "}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if info.Title != "Auth refactor" {
		t.Errorf("expected session_info name, got %q", info.Title)
	}
	// Slug derives from final title (session_info.name overrides user msg).
	if info.Slug != "auth-refactor" {
		t.Errorf("expected slug from session name, got %q", info.Slug)
	}
}

func TestParseSessionFileNoMessages(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if info.Title != "(new)" {
		t.Errorf("expected '(new)', got %q", info.Title)
	}
}

func TestParseSessionFileLongTitleTruncated(t *testing.T) {
	long := "Please help me with this very long request that goes on and on about many different things and really should be truncated for the sidebar"
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"`+long+`"}]}}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if len(info.Title) > 85 {
		t.Errorf("title too long: %q", info.Title)
	}
}

func TestParseSessionFileStringContent(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":"Help me debug this"}}`,
	)
	info, _ := NewPi().ParseSessionFile(path)
	if info.Title != "Help me debug this" {
		t.Errorf("expected string content as title, got %q", info.Title)
	}
}

// --- FileMonitor ---

func TestParseNewLinesNameChange(t *testing.T) {
	events := NewPi().ParseNewLines([]string{
		`{"type":"session_info","name":"My new name"}`,
	}, "")
	if len(events) != 1 || events[0].Title != "My new name" {
		t.Errorf("expected 1 title event, got %v", events)
	}
}

func TestParseNewLinesUserMessage(t *testing.T) {
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"Fix the bug"}]}}`,
	}, "")
	// Should produce: working status only (title comes from ParseSessionFile on attribution)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (status), got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true status")
	}
}

func TestParseNewLinesNameDoesNotAffectStatus(t *testing.T) {
	// session_info (name) entries must not emit any status event.
	// This ensures /name during an agent turn doesn't clear working state.
	events := NewPi().ParseNewLines([]string{
		`{"type":"session_info","name":"My project"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Title != "My project" {
		t.Errorf("expected title 'My project', got %q", events[0].Title)
	}
	if events[0].Status != nil {
		t.Error("session_info must NOT produce a status event — would clear working state")
	}
}

func TestParseNewLinesNameAmidToolUse(t *testing.T) {
	// Simulates /name during an agent turn: the batch contains toolUse messages
	// and a session_info entry. Title should change; working should remain true.
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
		`{"type":"message","id":"tr1","message":{"role":"toolResult","content":""}}`,
		`{"type":"session_info","name":"Refactoring auth"}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
	}, "")
	// toolUse events emit working=true, session_info emits title.
	var hasTitle bool
	var lastWorking *bool
	for _, e := range events {
		if e.Title == "Refactoring auth" {
			hasTitle = true
		}
		if e.Status != nil {
			w := e.Status.Working
			lastWorking = &w
		}
	}
	if !hasTitle {
		t.Error("expected title event 'Refactoring auth'")
	}
	if lastWorking == nil || !*lastWorking {
		t.Error("expected last status to be working=true (toolUse keeps working)")
	}
}

func TestParseNewLinesAssistantStop(t *testing.T) {
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false status on stop")
	}
	if events[0].Unread == nil || !*events[0].Unread {
		t.Error("expected unread=true on stop (turn complete)")
	}
}

func TestParseNewLinesAssistantToolUse(t *testing.T) {
	// toolUse stopReason means assistant is still working — emit working=true.
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event for toolUse, got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true for toolUse (agent loop continues)")
	}
}

func TestParseNewLinesAssistantAborted(t *testing.T) {
	// User pressed Esc — agent is idle.
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"aborted","content":[]}}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false on aborted")
	}
	// Aborted = user-initiated cancel, not a completed turn. No unread.
	if events[0].Unread != nil {
		t.Error("expected unread=nil on aborted (user cancelled, no new content)")
	}
}

func TestParseNewLinesAssistantErrorSingle(t *testing.T) {
	// A single error should NOT change state — a retry is expected.
	// The file has only 1 error, well below the exhausted threshold.
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"fix bug"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	}, path)
	if len(events) != 0 {
		t.Fatalf("expected 0 events for single error (retry pending), got %d", len(events))
	}
}

func TestParseNewLinesAssistantErrorExhausted(t *testing.T) {
	// Use a temp cwd with explicit maxRetries=3 (exhaustion at 4 errors)
	// so the test doesn't depend on the developer's ~/.pi config.
	cwd := t.TempDir()
	piDir := filepath.Join(cwd, ".pi")
	os.MkdirAll(piDir, 0o755)
	os.WriteFile(filepath.Join(piDir, "settings.json"), []byte(`{"retry":{"maxRetries":3}}`), 0o644)
	// 4 consecutive errors = retries exhausted. The agent gave up;
	// emit error (not working, error=true for red dot).
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"`+cwd+`"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"fix bug"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a3","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a4","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a4","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	}, path)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (exhausted → error), got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false after exhausted retries")
	}
	if !events[0].Status.Error {
		t.Error("expected error=true after exhausted retries")
	}
}

func TestParseNewLinesErrorExhaustedIgnoresCustomEvents(t *testing.T) {
	// Use a temp cwd with explicit maxRetries=3 so we don't read ~/.pi config.
	cwd := t.TempDir()
	piDir := filepath.Join(cwd, ".pi")
	os.MkdirAll(piDir, 0o755)
	os.WriteFile(filepath.Join(piDir, "settings.json"), []byte(`{"retry":{"maxRetries":3}}`), 0o644)
	// Custom/extension events between errors should not break the count.
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"`+cwd+`"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"fix bug"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"custom","customType":"jj-checkpoint","data":{}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"label","id":"l1","label":"jj:abc"}`,
		`{"type":"message","id":"a3","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"custom","customType":"jj-checkpoint","data":{}}`,
		`{"type":"message","id":"a4","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a4","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	}, path)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (exhausted), got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false after 4 errors with interleaved custom events")
	}
	if !events[0].Status.Error {
		t.Error("expected error=true after 4 errors with interleaved custom events")
	}
}

func TestParseNewLinesErrorAutoRetry(t *testing.T) {
	// Error followed by automatic retry (toolUse) in the same batch.
	// The error produces no state change (only 1 in file); toolUse re-asserts working.
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"fix bug"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
	)
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
	}, path)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (toolUse only), got %d", len(events))
	}
	if events[0].Status == nil || !events[0].Status.Working {
		t.Error("expected working=true (retry continues)")
	}
}

func TestParseNewLinesErrorNoFilePath(t *testing.T) {
	// When no file path is available (empty string), error should not
	// change state (safe default: assume retry is coming).
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	}, "")
	if len(events) != 0 {
		t.Fatalf("expected 0 events with no file path, got %d", len(events))
	}
}

func TestParseNewLinesErrorRespectsCustomRetryConfig(t *testing.T) {
	// When project-level settings set maxRetries=1, exhaustion threshold
	// is 2 (1 original + 1 retry). Two consecutive errors should go idle.
	dir := t.TempDir()

	// Write project-level pi settings with maxRetries=1.
	piDir := filepath.Join(dir, ".pi")
	os.MkdirAll(piDir, 0o755)
	os.WriteFile(filepath.Join(piDir, "settings.json"),
		[]byte(`{"retry":{"maxRetries":1}}`), 0o644)

	path := filepath.Join(dir, "session.jsonl")
	var content string
	for _, line := range []string{
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"` + dir + `"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":"fix bug"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	} {
		content += line + "\n"
	}
	os.WriteFile(path, []byte(content), 0o644)

	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	}, path)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (exhausted with maxRetries=1), got %d", len(events))
	}
	if events[0].Status == nil || events[0].Status.Working {
		t.Error("expected working=false after exhausted retries with custom config")
	}
	if !events[0].Status.Error {
		t.Error("expected error=true after exhausted retries with custom config")
	}
}

func TestParseNewLinesFullTurnCycle(t *testing.T) {
	// Complete turn: user → toolUse → toolUse → stop
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
		`{"type":"message","id":"tr1","message":{"role":"toolResult","content":""}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
		`{"type":"message","id":"tr2","message":{"role":"toolResult","content":""}}`,
		`{"type":"message","id":"a3","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`,
	}, "")
	// user=working, toolUse=working, toolUse=working, stop=idle
	// (toolResult has no events)
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	// Last event must be idle.
	last := events[len(events)-1]
	if last.Status == nil || last.Status.Working {
		t.Error("last event should be idle (stop)")
	}
}

func TestParseNewLinesIgnoresNonMessageTypes(t *testing.T) {
	// All non-message, non-session_info types should be silently ignored.
	events := NewPi().ParseNewLines([]string{
		`{"type":"text","id":"t1","text":"some output"}`,
		`{"type":"toolCall","id":"tc1","name":"bash"}`,
		`{"type":"thinking","id":"th1","text":"let me think"}`,
		`{"type":"model_change","id":"mc1","provider":"anthropic"}`,
		`{"type":"compaction","id":"c1"}`,
		`{"type":"branch_summary","id":"bs1"}`,
		`{"type":"thinking_level_change","id":"tl1","thinkingLevel":"high"}`,
		`{"type":"custom_message","id":"cm1"}`,
		`{"type":"image","id":"i1"}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for non-message types, got %d", len(events))
	}
}

func TestParseNewLinesUnknownStopReason(t *testing.T) {
	// Unknown stopReasons (e.g. from future protocol versions) must not
	// change state. This prevents extensions or new features from
	// accidentally clearing the working indicator.
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"someNewReason","content":[]}}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown stopReason, got %d", len(events))
	}
}

func TestParseNewLinesCustomExtensionEvents(t *testing.T) {
	// Extensions can emit custom event types. These must be silently
	// ignored and never disrupt the current state.
	events := NewPi().ParseNewLines([]string{
		`{"type":"extension_progress","id":"ep1","progress":0.5}`,
		`{"type":"custom_diagnostic","severity":"warning","message":"slow query"}`,
		`{"type":"metrics","cpu":42,"memory":1024}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for custom extension types, got %d", len(events))
	}
}

func TestParseNewLinesToolResult(t *testing.T) {
	// toolResult messages should not generate events
	events := NewPi().ParseNewLines([]string{
		`{"type":"message","id":"tr1","message":{"role":"toolResult","content":""}}`,
	}, "")
	if len(events) != 0 {
		t.Errorf("expected 0 events for toolResult, got %d", len(events))
	}
}

// --- Resumer ---

func TestResumeCommand(t *testing.T) {
	cmd := NewPi().ResumeCommand(&adapter.SessionFileInfo{
		FilePath: "/tmp/test.jsonl",
	})
	if len(cmd) != 4 || cmd[0] != "pi" || cmd[1] != "--session" || cmd[3] != "-c" {
		t.Errorf("unexpected resume command: %v", cmd)
	}
}

func TestCanResume(t *testing.T) {
	valid := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	)
	if !NewPi().CanResume(valid) {
		t.Fatal("should be resumable")
	}

	empty := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
	)
	if NewPi().CanResume(empty) {
		t.Fatal("empty session should not be resumable")
	}
}

// --- Helpers ---

func TestSessionDirEncoding(t *testing.T) {
	dir := NewPi().SessionDir("/home/mg/dev/gmux")
	if base := filepath.Base(dir); base != "--home-mg-dev-gmux--" {
		t.Errorf("expected --home-mg-dev-gmux--, got %s", base)
	}
}

func TestSessionRootDirRespectsEnvVar(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	root := NewPi().SessionRootDir()
	want := filepath.Join(custom, "sessions")
	if root != want {
		t.Errorf("expected %s, got %s", want, root)
	}
}

func TestSessionRootDirDefaultWithoutEnvVar(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	root := NewPi().SessionRootDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".pi", "agent", "sessions")
	if root != want {
		t.Errorf("expected %s, got %s", want, root)
	}
}

func TestListSessionFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "b.jsonl"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("nope"), 0644)
	if len(ListSessionFiles(dir)) != 2 {
		t.Fatal("expected 2 jsonl files")
	}
}

// --- extractPiText ---

func TestExtractPiTextIncludesToolResult(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"run tests"}]}}`,
		`{"type":"message","id":"tr1","message":{"role":"toolResult","content":"PASS: 5 tests passed"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"All tests pass."}]}}`,
	)
	text, err := extractPiText(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "run tests") {
		t.Error("missing user message text")
	}
	if !strings.Contains(text, "PASS: 5 tests passed") {
		t.Error("missing toolResult text")
	}
	if !strings.Contains(text, "All tests pass") {
		t.Error("missing assistant text")
	}
}
