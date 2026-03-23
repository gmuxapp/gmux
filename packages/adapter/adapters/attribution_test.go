package adapters

import (
	"os"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- similarityScore ---

func TestSimilarityScoreExactMatch(t *testing.T) {
	score := similarityScore("hello world", "hello world")
	if score < 0.99 {
		t.Fatalf("expected ~1.0 for exact match, got %f", score)
	}
}

func TestSimilarityScorePartialMatch(t *testing.T) {
	score := similarityScore("fix the bug", "Let me fix the bug for you and also add tests")
	if score < 0.9 {
		t.Fatalf("expected high score for substring match, got %f", score)
	}
}

func TestSimilarityScoreNoMatch(t *testing.T) {
	score := similarityScore("aaaaa bbbbb ccccc", "xxxxx yyyyy zzzzz")
	if score > 0.2 {
		t.Fatalf("expected low score for no overlap, got %f", score)
	}
}

func TestSimilarityScoreEmpty(t *testing.T) {
	if similarityScore("", "hello") != 0 {
		t.Fatal("expected 0 for empty file tail")
	}
	if similarityScore("hello", "") != 0 {
		t.Fatal("expected 0 for empty scrollback")
	}
}

// --- longestCommonSubstring ---

func TestLongestCommonSubstring(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"abcdef", "xbcdey", 4},
		{"hello", "world", 1},
		{"", "abc", 0},
		{"same", "same", 4},
		{"abc", "xyz", 0},
	}
	for _, tt := range tests {
		got := longestCommonSubstring(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("lcs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// --- tail ---

func TestTail(t *testing.T) {
	if tail("hello world", 5) != "world" {
		t.Fatal("expected 'world'")
	}
	if tail("hi", 10) != "hi" {
		t.Fatal("expected 'hi' when n > len")
	}
}

// --- attributeByScrollback ---

func TestAttributeByScrollbackSingleMatch(t *testing.T) {
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: "completely different text"},
		{SessionID: "b", Scrollback: "Let me fix the auth bug for you"},
	}
	id := attributeByScrollback("fix the auth bug", candidates)
	if id != "b" {
		t.Fatalf("expected 'b', got %q", id)
	}
}

func TestAttributeByScrollbackNoMatch(t *testing.T) {
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: "aaaaa bbbbb"},
		{SessionID: "b", Scrollback: "ccccc ddddd"},
	}
	id := attributeByScrollback("xxxxx yyyyy zzzzz", candidates)
	if id != "" {
		t.Fatalf("expected empty (no match), got %q", id)
	}
}

func TestAttributeByScrollbackEmptyFile(t *testing.T) {
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: "hello"},
	}
	if id := attributeByScrollback("", candidates); id != "" {
		t.Fatalf("expected empty, got %q", id)
	}
}

func TestAttributeByScrollbackNoScrollback(t *testing.T) {
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: ""},
		{SessionID: "b", Scrollback: ""},
	}
	if id := attributeByScrollback("hello world", candidates); id != "" {
		t.Fatalf("expected empty, got %q", id)
	}
}

// --- attributeByScrollbackNormalized ---

func TestAttributeByScrollbackNormalizedCleaning(t *testing.T) {
	// File has markdown backticks, double spaces, and newlines.
	// Scrollback has box-drawing borders and collapsed whitespace.
	// After cleaning, the underlying text should match.
	candidates := []adapter.FileCandidate{
		{SessionID: "wrong", Scrollback: "completely unrelated text about something else entirely and more words"},
		{SessionID: "right", Scrollback: "───── Working copy (@) now at: abc123 Committed as def456 ─────"},
	}
	fileText := "Working copy  (@) now at: abc123\nCommitted as `def456`."
	id := attributeByScrollbackNormalized(fileText, candidates)
	if id != "right" {
		t.Fatalf("expected 'right' (cleaned match), got %q", id)
	}
}

func TestAttributeByScrollbackNormalizedRejectsShort(t *testing.T) {
	// Very short file text (< 20 chars after cleaning) should be rejected
	// to avoid false matches on content like "hi" or "ok".
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: "hi there how are you doing today"},
	}
	id := attributeByScrollbackNormalized("hi", candidates)
	if id != "" {
		t.Fatalf("expected empty (too short), got %q", id)
	}
}

func TestCleanForMatching(t *testing.T) {
	got := cleanForMatching("──── `hello`  **world**\n\tfoo ────")
	if got != "hello world foo" {
		t.Fatalf("expected cleaned text, got %q", got)
	}
}

// --- attributeByMetadata ---

func TestAttributeByMetadataExactMatch(t *testing.T) {
	now := time.Now()
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Cwd: "/home/user/project-a", StartedAt: now.Add(-10 * time.Second)},
		{SessionID: "b", Cwd: "/home/user/project-b", StartedAt: now.Add(-5 * time.Second)},
	}
	info := &adapter.SessionFileInfo{
		Cwd:     "/home/user/project-b",
		Created: now,
	}
	id := attributeByMetadata(info, candidates)
	if id != "b" {
		t.Fatalf("expected 'b', got %q", id)
	}
}

func TestAttributeByMetadataSameCwdPicksClosest(t *testing.T) {
	now := time.Now()
	candidates := []adapter.FileCandidate{
		{SessionID: "old", Cwd: "/home/user", StartedAt: now.Add(-10 * time.Minute)},
		{SessionID: "new", Cwd: "/home/user", StartedAt: now.Add(-2 * time.Second)},
	}
	info := &adapter.SessionFileInfo{
		Cwd:     "/home/user",
		Created: now,
	}
	id := attributeByMetadata(info, candidates)
	if id != "new" {
		t.Fatalf("expected 'new', got %q", id)
	}
}

func TestAttributeByMetadataCwdMismatch(t *testing.T) {
	now := time.Now()
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Cwd: "/home/user/project-a", StartedAt: now},
	}
	info := &adapter.SessionFileInfo{
		Cwd:     "/home/user/project-b",
		Created: now,
	}
	id := attributeByMetadata(info, candidates)
	if id != "" {
		t.Fatalf("expected empty (cwd mismatch), got %q", id)
	}
}

func TestAttributeByMetadataTooOld(t *testing.T) {
	now := time.Now()
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Cwd: "/home/user", StartedAt: now.Add(-10 * time.Minute)},
	}
	info := &adapter.SessionFileInfo{
		Cwd:     "/home/user",
		Created: now,
	}
	id := attributeByMetadata(info, candidates)
	if id != "" {
		t.Fatalf("expected empty (>5min delta), got %q", id)
	}
}

func TestAttributeByMetadataNilInfo(t *testing.T) {
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Cwd: "/home/user"},
	}
	if id := attributeByMetadata(nil, candidates); id != "" {
		t.Fatalf("expected empty, got %q", id)
	}
}

// --- Pi AttributeFile ---

func TestPiAttributeFileContentMatch(t *testing.T) {
	// Scrollback has overlapping content with tool output in the file.
	// This is the primary attribution mechanism for pi.
	candidates := []adapter.FileCandidate{
		{SessionID: "wrong", Scrollback: "completely unrelated text about cooking recipes and more words to fill space"},
		{SessionID: "right", Scrollback: "───── Working copy (@) now at: sxpovqxo 395e26fa Committed as 05c82cde ─────"},
	}
	pi := NewPi()
	dir := t.TempDir()
	path := dir + "/test.jsonl"
	content := `{"type":"session","id":"s1","cwd":"/tmp","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","id":"tr1","message":{"role":"toolResult","content":"Working copy  (@) now at: sxpovqxo 395e26fa (empty)\nParent commit (@-)      : orozwtmt 05c82cde"}}
{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"Committed as 05c82cde."}]}}
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	id := pi.AttributeFile(path, candidates)
	if id != "right" {
		t.Fatalf("expected 'right' (content match), got %q", id)
	}
}

func TestPiAttributeFileNoScrollback(t *testing.T) {
	// No scrollback available: return "" (session may be idle or just started).
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: ""},
		{SessionID: "b", Scrollback: ""},
	}
	pi := NewPi()
	dir := t.TempDir()
	path := dir + "/test.jsonl"
	content := `{"type":"session","id":"s1","cwd":"/tmp","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix the auth bug in the login handler please"}]}}
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	id := pi.AttributeFile(path, candidates)
	if id != "" {
		t.Fatalf("expected empty (no scrollback), got %q", id)
	}
}

func TestPiAttributeFileRejectsShortContent(t *testing.T) {
	// Very short file content should be rejected to avoid false matches.
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Scrollback: "hi there how are you doing today"},
	}
	pi := NewPi()
	dir := t.TempDir()
	path := dir + "/test.jsonl"
	content := `{"type":"session","id":"s1","cwd":"/tmp","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","id":"u1","message":{"role":"user","content":"hi"}}
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	id := pi.AttributeFile(path, candidates)
	if id != "" {
		t.Fatalf("expected empty (content too short for reliable match), got %q", id)
	}
}

// --- Codex AttributeFile ---

func TestCodexAttributeFile(t *testing.T) {
	now := time.Now()
	candidates := []adapter.FileCandidate{
		{SessionID: "wrong", Cwd: "/home/user/other", StartedAt: now},
		{SessionID: "right", Cwd: "/home/user/project", StartedAt: now.Add(-3 * time.Second)},
	}
	codex := NewCodex()
	dir := t.TempDir()
	path := dir + "/rollout-test.jsonl"
	content := `{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"abc-123","timestamp":"` + now.Format(time.RFC3339Nano) + `","cwd":"/home/user/project"}}
{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	id := codex.AttributeFile(path, candidates)
	if id != "right" {
		t.Fatalf("expected 'right', got %q", id)
	}
}

func TestCodexAttributeFileNoCwdMatch(t *testing.T) {
	now := time.Now()
	candidates := []adapter.FileCandidate{
		{SessionID: "a", Cwd: "/home/user/other", StartedAt: now},
	}
	codex := NewCodex()
	dir := t.TempDir()
	path := dir + "/rollout-test.jsonl"
	content := `{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"abc","timestamp":"` + now.Format(time.RFC3339Nano) + `","cwd":"/home/user/project"}}
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	id := codex.AttributeFile(path, candidates)
	if id != "" {
		t.Fatalf("expected empty (cwd mismatch), got %q", id)
	}
}

// --- Claude AttributeFile ---

func TestClaudeAttributeFile(t *testing.T) {
	now := time.Now()

	candidates := []adapter.FileCandidate{
		{SessionID: "old", Cwd: "/home/user/project", StartedAt: now.Add(-10 * time.Minute)},
		{SessionID: "new", Cwd: "/home/user/project", StartedAt: now.Add(-1 * time.Second)},
	}
	claude := NewClaude()
	dir := t.TempDir()
	path := dir + "/test-session.jsonl"
	content := `{"type":"user","sessionId":"sess-abc","cwd":"/home/user/project","timestamp":"` + now.Format(time.RFC3339Nano) + `","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
`
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	id := claude.AttributeFile(path, candidates)
	if id != "new" {
		t.Fatalf("expected 'new', got %q", id)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
