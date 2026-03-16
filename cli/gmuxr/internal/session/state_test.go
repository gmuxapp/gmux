package session

import (
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestNewState(t *testing.T) {
	s := New(Config{
		ID:         "sess-test",
		Command:    []string{"echo", "hello"},
		Cwd:        "/tmp",
		Kind:       "generic",
		SocketPath: "/tmp/gmux-sessions/sess-test.sock",
		Title:      "echo hello",
	})

	if s.ID != "sess-test" {
		t.Fatalf("expected 'sess-test', got %q", s.ID)
	}
	if s.Alive {
		t.Fatal("new state should not be alive")
	}
	if s.Kind != "generic" {
		t.Fatalf("expected 'generic', got %q", s.Kind)
	}
}

func TestSetRunning(t *testing.T) {
	s := New(Config{ID: "sess-1", Command: []string{"echo"}, Kind: "generic"})
	s.SetRunning(12345)

	if !s.Alive {
		t.Fatal("should be alive after SetRunning")
	}
	if s.Pid != 12345 {
		t.Fatalf("expected pid 12345, got %d", s.Pid)
	}
	if s.StartedAt == "" {
		t.Fatal("started_at should be set")
	}
}

func TestSetExited(t *testing.T) {
	s := New(Config{ID: "sess-1", Command: []string{"echo"}, Kind: "generic"})
	s.SetRunning(12345)
	s.SetExited(42)

	if s.Alive {
		t.Fatal("should not be alive after SetExited")
	}
	if s.ExitCode == nil || *s.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %v", s.ExitCode)
	}
	if s.ExitedAt == "" {
		t.Fatal("exited_at should be set")
	}
}

func TestSetStatus(t *testing.T) {
	s := New(Config{ID: "sess-1", Command: []string{"echo"}, Kind: "generic"})
	s.SetStatus(&adapter.Status{Label: "thinking", Working: true})

	if s.Status == nil {
		t.Fatal("status should be set")
	}
	if s.Status.Label != "thinking" {
		t.Fatalf("expected 'thinking', got %q", s.Status.Label)
	}
}

func TestPatchMeta(t *testing.T) {
	s := New(Config{ID: "sess-1", Command: []string{"echo"}, Title: "original", Kind: "generic"})

	newTitle := "updated"
	s.PatchMeta(&newTitle, nil)
	if s.Title != "updated" {
		t.Fatalf("expected 'updated', got %q", s.Title)
	}

	newSub := "sub"
	s.PatchMeta(nil, &newSub)
	if s.Title != "updated" {
		t.Fatal("title should not change when patching only subtitle")
	}
	if s.Subtitle != "sub" {
		t.Fatalf("expected 'sub', got %q", s.Subtitle)
	}
}

func TestShellTitleUsedAsFallbackUntilAdapterTitleArrives(t *testing.T) {
	s := New(Config{ID: "sess-1", Command: []string{"pi"}, Title: "pi", Kind: "pi"})

	s.SetShellTitle("~/dev/project")
	if s.Title != "~/dev/project" {
		t.Fatalf("expected shell fallback title, got %q", s.Title)
	}

	s.SetAdapterTitle("fix auth bug")
	if s.Title != "fix auth bug" {
		t.Fatalf("expected adapter title to win, got %q", s.Title)
	}

	s.SetShellTitle("~/dev/other")
	if s.Title != "fix auth bug" {
		t.Fatalf("expected adapter title to keep winning, got %q", s.Title)
	}
}

func TestClearingAdapterTitleRevealsShellTitle(t *testing.T) {
	s := New(Config{ID: "sess-1", Command: []string{"pi"}, Title: "pi", Kind: "pi"})

	s.SetShellTitle("~/dev/project")
	s.SetAdapterTitle("named task")
	if s.Title != "named task" {
		t.Fatalf("expected adapter title, got %q", s.Title)
	}

	empty := ""
	s.PatchMeta(&empty, nil)
	if s.Title != "~/dev/project" {
		t.Fatalf("expected shell title after clearing adapter title, got %q", s.Title)
	}
}

func TestJSON(t *testing.T) {
	s := New(Config{
		ID:         "sess-json",
		Command:    []string{"pi"},
		Cwd:        "/home/user",
		Kind:       "pi",
		SocketPath: "/tmp/gmux-sessions/sess-json.sock",
		Title:      "test session",
	})
	s.SetRunning(999)
	s.SetStatus(&adapter.Status{Label: "working", Working: true})

	data, err := s.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if parsed["id"] != "sess-json" {
		t.Fatalf("expected 'sess-json', got %v", parsed["id"])
	}
	if parsed["alive"] != true {
		t.Fatalf("expected alive=true, got %v", parsed["alive"])
	}
	if parsed["kind"] != "pi" {
		t.Fatalf("expected 'pi', got %v", parsed["kind"])
	}

	status := parsed["status"].(map[string]interface{})
	if status["label"] != "working" {
		t.Fatalf("expected 'working', got %v", status["label"])
	}
}

func TestSubscribeEvents(t *testing.T) {
	s := New(Config{ID: "sess-sub", Command: []string{"echo"}, Kind: "generic"})

	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	s.SetStatus(&adapter.Status{Label: "test"})

	evt := <-ch
	if evt.Type != "status" {
		t.Fatalf("expected 'status' event, got %q", evt.Type)
	}
}

func TestUnsubscribe(t *testing.T) {
	s := New(Config{ID: "sess-unsub", Command: []string{"echo"}, Kind: "generic"})
	ch := s.Subscribe()
	s.Unsubscribe(ch)

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
}
