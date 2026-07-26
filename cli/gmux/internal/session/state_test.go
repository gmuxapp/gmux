package session

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestNewState(t *testing.T) {
	s := New(Config{
		ID:         "sess-test",
		Command:    []string{"echo", "hello"},
		Cwd:        "/tmp",
		Adapter:    "generic",
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
	s := New(Config{ID: "s", Command: []string{"/usr/bin/pi"}, Adapter: "pi"})
	if s.Title() != "pi" {
		t.Fatalf("expected 'pi', got %q", s.Title())
	}
}

func TestShellTitleBeforeAdapterTitle(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"pi"}, Adapter: "pi"})

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
	s := New(Config{ID: "s", Command: []string{"pi"}, Adapter: "pi"})

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
	s := New(Config{ID: "s", Command: []string{"echo"}, Adapter: "generic"})
	s.SetRunning(12345)

	if !s.Alive {
		t.Fatal("should be alive")
	}
	if s.Pid != 12345 {
		t.Fatalf("expected pid 12345, got %d", s.Pid)
	}
}

func TestSetExited(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Adapter: "generic"})
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
	s := New(Config{ID: "s", Command: []string{"echo"}, Adapter: "generic"})
	s.SetStatus(&adapter.Status{Active: true, Error: true})

	if s.Status == nil || !s.Status.Active || !s.Status.Error {
		t.Fatalf("expected active+error, got %v", s.Status)
	}
}

// TestCloseTurnIsAtomic: the terminal-end check and write are one critical
// section. Hook POSTs are served on independent goroutines, so a
// StatusSnapshot-then-SetStatus caller could let two ends both observe an open
// turn and both write — and now that Interrupted/Error are durable, the loser
// would rewrite the winner's closure. Exactly one close may win, and the
// surviving status must be that winner's.
func TestCloseTurnIsAtomic(t *testing.T) {
	for range 200 {
		s := New(Config{ID: "s", Command: []string{"pi"}, Adapter: "pi"})
		s.SetStatus(&adapter.Status{Active: true})

		const racers = 8
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(racers)
		won := make([]bool, racers)
		for i := range racers {
			go func() {
				defer done.Done()
				start.Wait()
				// Each racer writes a distinguishable closure.
				won[i] = s.CloseTurn(&adapter.Status{Interrupted: i%2 == 0, Error: i%2 == 1})
			}()
		}
		start.Done()
		done.Wait()

		winners := 0
		for _, w := range won {
			if w {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("CloseTurn winners = %d, want exactly 1", winners)
		}
		got := s.StatusSnapshot()
		if got == nil || got.Active {
			t.Fatalf("status = %+v, want a closed turn", got)
		}
		if got.Interrupted == got.Error {
			t.Fatalf("status = %+v, want exactly one racer's closure", got)
		}
		// A closed turn stays closed for every later end.
		if s.CloseTurn(&adapter.Status{}) {
			t.Fatal("CloseTurn succeeded against an already-closed turn")
		}
	}
}

// TestCloseTurnRequiresReportedOpenTurn: a session whose runner never reported
// a status has no open turn, so an end must not fabricate one.
func TestCloseTurnRequiresReportedOpenTurn(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"pi"}, Adapter: "pi"})
	if s.CloseTurn(&adapter.Status{Interrupted: true}) {
		t.Fatal("CloseTurn must fail with no reported status")
	}
	if s.StatusSnapshot() != nil {
		t.Fatalf("status = %+v, want it left unreported", s.StatusSnapshot())
	}
}

func TestJSONIncludesComputedTitle(t *testing.T) {
	s := New(Config{
		ID:      "sess-json",
		Command: []string{"pi"},
		Cwd:     "/home/user",
		Adapter: "pi",
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
	s := New(Config{ID: "s", Command: []string{"echo"}, Adapter: "generic"})
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	s.SetStatus(&adapter.Status{Active: true})

	evt := <-ch
	if evt.Type != "status" {
		t.Fatalf("expected 'status', got %q", evt.Type)
	}
}

func TestUnsubscribe(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Adapter: "generic"})
	ch := s.Subscribe()
	s.Unsubscribe(ch)

	_, ok := <-ch
	if ok {
		t.Fatal("channel should be closed")
	}
}

func TestEmitActivityThrottles(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"echo"}, Adapter: "generic"})
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

// TestBindSlugAlwaysEmits pins the divergence-recovery contract behind the
// runner→daemon slug sync. SetSlug dedups (no event when unchanged), which is
// right for a same-conversation refresh where runner and daemon agree. But on
// an authoritative re-bind after a daemon re-register — where the daemon may
// hold a STALE slug while this fresh runner state is empty — a dedup'd clear
// would never reach the daemon. BindSlug therefore always emits, even when the
// value is unchanged, so the daemon converges.
func TestBindSlugAlwaysEmits(t *testing.T) {
	s := New(Config{ID: "sess-x", Adapter: "pi", SocketPath: "/tmp/x.sock"})
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	slugEvents := func() []string {
		var got []string
		for {
			select {
			case e := <-ch:
				if e.Type != "meta" {
					continue
				}
				if m, ok := e.Data.(map[string]string); ok {
					if v, present := m["slug"]; present {
						got = append(got, v)
					}
				}
			default:
				return got
			}
		}
	}

	// SetSlug on an unchanged (empty→empty) value must NOT emit.
	s.SetSlug("")
	if got := slugEvents(); len(got) != 0 {
		t.Fatalf("SetSlug(unchanged) emitted %v, want nothing", got)
	}

	// BindSlug on the same unchanged empty value MUST emit — this is the
	// re-register clear that would otherwise be lost.
	s.BindSlug("")
	if got := slugEvents(); len(got) != 1 || got[0] != "" {
		t.Fatalf("BindSlug(\"\") emitted %v, want one empty-slug event", got)
	}

	// And it keeps runner state consistent.
	if s.SlugSnapshot() != "" {
		t.Fatalf("SlugSnapshot = %q, want empty", s.SlugSnapshot())
	}
}
