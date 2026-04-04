package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalExcludesInternalFields(t *testing.T) {
	s := Session{
		ID:           "test-1",
		Kind:         "pi",
		Alive:        true,
		Title:        "Implement adapter system",
		Resumable:    true,
		Stale:        true,
		ShellTitle:   "user@host:~/dev",
		AdapterTitle: "Implement adapter system",
		ResumeKey:    "session-file-id",
		BinaryHash:   "abc123",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)

	// Internal fields must not appear.
	for _, field := range []string{"shell_title", "adapter_title", "resume_key", "binary_hash"} {
		if strings.Contains(out, field) {
			t.Errorf("MarshalJSON should exclude %s, got: %s", field, out)
		}
	}
	// Their derived outputs must appear.
	for _, field := range []string{"title", "resumable", "stale"} {
		if !strings.Contains(out, field) {
			t.Errorf("MarshalJSON should include %s, got: %s", field, out)
		}
	}
	t.Logf("Output: %s", out)
}

func TestUnmarshalPreservesInternalFields(t *testing.T) {
	input := `{"id":"test-1","kind":"pi","alive":true,"title":"resolved","shell_title":"terminal","adapter_title":"from adapter","resume_key":"file-id","binary_hash":"abc"}`
	var s Session
	if err := json.Unmarshal([]byte(input), &s); err != nil {
		t.Fatal(err)
	}
	if s.ShellTitle != "terminal" {
		t.Errorf("expected ShellTitle='terminal', got %q", s.ShellTitle)
	}
	if s.AdapterTitle != "from adapter" {
		t.Errorf("expected AdapterTitle='from adapter', got %q", s.AdapterTitle)
	}
	if s.ResumeKey != "file-id" {
		t.Errorf("expected ResumeKey='file-id', got %q", s.ResumeKey)
	}
	if s.BinaryHash != "abc" {
		t.Errorf("expected BinaryHash='abc', got %q", s.BinaryHash)
	}
	if s.Title != "resolved" {
		t.Errorf("expected Title='resolved', got %q", s.Title)
	}
}

func TestEventMarshalExcludesInternalFields(t *testing.T) {
	sess := Session{
		ID:           "test-1",
		Kind:         "pi",
		Alive:        true,
		Title:        "fix bug",
		AdapterTitle: "fix bug",
		ShellTitle:   "user@host",
		ResumeKey:    "file-id",
		BinaryHash:   "abc",
	}
	ev := Event{
		Type:    "session-upsert",
		ID:      "test-1",
		Session: &sess,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, field := range []string{"shell_title", "adapter_title", "resume_key", "binary_hash"} {
		if strings.Contains(out, field) {
			t.Errorf("Event marshal should exclude %s, got: %s", field, out)
		}
	}
}
