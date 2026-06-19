package adapters

import (
	"os"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

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
