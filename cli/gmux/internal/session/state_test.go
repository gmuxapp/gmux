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
	})

	if s.ID != "sess-test" {
		t.Fatalf("expected 'sess-test', got %q", s.ID)
	}
	if s.Alive {
		t.Fatal("new state should not be alive")
	}
	if s.Title() != "echo hello" {
		t.Fatalf("expected 'echo hello', got %q", s.Title())
	}
}

func TestTitleFallsBackToCommandBasename(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"/usr/bin/pi"}, Kind: "pi"})
	if s.Title() != "pi" {
		t.Fatalf("expected 'pi', got %q", s.Title())
	}
}

func TestShellTitleBeforeAdapterTitle(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"pi"}, Kind: "pi"})

	s.SetShellTitle("~/dev/project")
	if s.Title() != "~/dev/project" {
		t.Fatalf("expected shell title, got %q", s.Title())
	}

	s.SetAdapterTitle("fix auth bug")
	if s.Title() != "fix auth bug" {
		t.Fatalf("expected adapter title to win, got %q", s.Title())
	}

	s.SetShellTitle("~/dev/other")
	if s.Title() != "fix auth bug" {
		t.Fatalf("adapter title should keep winning, got %q", s.Title())
	}
}

func TestClearAdapterTitleRevealsShellTitle(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"pi"}, Kind: "pi"})

	s.SetShellTitle("~/dev/project")
	s.SetAdapterTitle("named task")
	if s.Title() != "named task" {
		t.Fatalf("expected adapter title, got %q", s.Title())
	}

	s.SetAdapterTitle("")
	if s.Title() != "~/dev/project" {
		t.Fatalf("expected shell title after clearing adapter, got %q", s.Title())
	}
}

func TestSetRunning(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Kind: "generic"})
	s.SetRunning(12345)

	if !s.Alive {
		t.Fatal("should be alive")
	}
	if s.Pid != 12345 {
		t.Fatalf("expected pid 12345, got %d", s.Pid)
	}
}

func TestSetExited(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Kind: "generic"})
	s.SetRunning(12345)
	s.SetExited(42)

	if s.Alive {
		t.Fatal("should not be alive")
	}
	if s.ExitCode == nil || *s.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %v", s.ExitCode)
	}
}

func TestSetStatus(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Kind: "generic"})
	s.SetStatus(&adapter.Status{Label: "thinking", Working: true})

	if s.Status == nil || s.Status.Label != "thinking" {
		t.Fatalf("expected 'thinking', got %v", s.Status)
	}
}

func TestJSONIncludesComputedTitle(t *testing.T) {
	s := New(Config{
		ID:      "sess-json",
		Command: []string{"pi"},
		Cwd:     "/home/user",
		Kind:    "pi",
	})
	s.SetShellTitle("~/dev/gmux")

	data, err := s.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if parsed["title"] != "~/dev/gmux" {
		t.Fatalf("expected computed title '~/dev/gmux', got %v", parsed["title"])
	}
	if parsed["shell_title"] != "~/dev/gmux" {
		t.Fatalf("expected shell_title, got %v", parsed["shell_title"])
	}
}

func TestSubscribeEvents(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Kind: "generic"})
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	s.SetStatus(&adapter.Status{Label: "test"})

	evt := <-ch
	if evt.Type != "status" {
		t.Fatalf("expected 'status', got %q", evt.Type)
	}
}

func TestUnsubscribe(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Kind: "generic"})
	ch := s.Subscribe()
	s.Unsubscribe(ch)

	_, ok := <-ch
	if ok {
		t.Fatal("channel should be closed")
	}
}

func TestEmitActivityThrottles(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Kind: "generic"})
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	// First call should emit.
	s.EmitActivity()
	select {
	case evt := <-ch:
		if evt.Type != "activity" {
			t.Fatalf("expected 'activity' event, got %q", evt.Type)
		}
	default:
		t.Fatal("expected activity event from first call")
	}

	// Immediate second call should be throttled (no event).
	s.EmitActivity()
	select {
	case evt := <-ch:
		t.Fatalf("expected no event (throttled), got %q", evt.Type)
	default:
		// good, throttled
	}
}
