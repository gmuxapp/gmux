package session

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestUnreadResultTokenAdvancesWhileAlreadyUnread(t *testing.T) {
	s := New(Config{ID: "s"})
	s.MarkUnreadResult()
	first := s.UnreadToken
	s.MarkUnreadResult()
	if first == "" || s.UnreadToken == first || !s.UnreadSnapshot() {
		t.Fatalf("tokens first=%q second=%q unread=%v", first, s.UnreadToken, s.UnreadSnapshot())
	}
	if s.AcknowledgeUnread(first) || !s.UnreadSnapshot() {
		t.Fatal("old token acknowledged a newer result")
	}
}

func TestNewState(t *testing.T) {
	s := New(Config{
		ID:         "1c54cqk8",
		Command:    []string{"echo", "hello"},
		Cwd:        "/tmp",
		Adapter:    "generic",
		SocketPath: "/tmp/gmux-sessions/1c54cqk8.sock",
	})

	if s.ID != "1c54cqk8" {
		t.Fatalf("expected '1c54cqk8', got %q", s.ID)
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
				won[i] = s.CloseTurnFrame(TurnClose{}, &adapter.Status{Interrupted: i%2 == 0, Error: i%2 == 1})
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
			t.Fatalf("CloseTurnFrame winners = %d, want exactly 1", winners)
		}
		got := s.StatusSnapshot()
		if got == nil || got.Active {
			t.Fatalf("status = %+v, want a closed turn", got)
		}
		if got.Interrupted == got.Error {
			t.Fatalf("status = %+v, want exactly one racer's closure", got)
		}
		// A closed turn stays closed for every later end.
		if s.CloseTurnFrame(TurnClose{}, &adapter.Status{}) {
			t.Fatal("CloseTurnFrame succeeded against an already-closed turn")
		}
	}
}

// TestCloseTurnRequiresReportedOpenTurn: a session whose runner never reported
// a status has no open turn, so an end must not fabricate one.
func TestCloseTurnRequiresReportedOpenTurn(t *testing.T) {
	s := New(Config{ID: "s", Command: []string{"pi"}, Adapter: "pi"})
	if s.CloseTurnFrame(TurnClose{}, &adapter.Status{Interrupted: true}) {
		t.Fatal("CloseTurnFrame must fail with no reported status")
	}
	if s.StatusSnapshot() != nil {
		t.Fatalf("status = %+v, want it left unreported", s.StatusSnapshot())
	}
}

func TestJSONIncludesComputedTitle(t *testing.T) {
	s := New(Config{
		ID:      "19cq55c5",
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
	s := New(Config{ID: "1108gm0e", Adapter: "pi", SocketPath: "/tmp/x.sock"})
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

// TestAdmitActionRequirements pins the activity semantics the runner's semantic
// actions are built on: activity is exactly Status.Active, so an errored turn
// that is still running is active, while a terminal error, an interruption and
// "nothing reported yet" are all inactive.
func TestAdmitActionRequirements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status *adapter.Status
		req    TurnRequirement
		want   Admission
	}{
		{"nothing reported is inactive", nil, RequireInactive, Admitted},
		{"nothing reported fails an active requirement", nil, RequireActive, RefusedInactive},
		{"running turn", &adapter.Status{Active: true}, RequireActive, Admitted},
		{"running turn refuses an inactive requirement", &adapter.Status{Active: true}, RequireInactive, RefusedActive},
		{"active+error is still active", &adapter.Status{Active: true, Error: true}, RequireInactive, RefusedActive},
		{"active+error satisfies an active requirement", &adapter.Status{Active: true, Error: true}, RequireActive, Admitted},
		{"terminal error is inactive", &adapter.Status{Error: true}, RequireInactive, Admitted},
		{"interrupted is inactive", &adapter.Status{Interrupted: true}, RequireInactive, Admitted},
		{"any ignores activity while busy", &adapter.Status{Active: true}, RequireAny, Admitted},
		{"any ignores activity while idle", nil, RequireAny, Admitted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{ID: "s1"})
			if tc.status != nil {
				s.SetStatus(tc.status)
			}
			if got, _ := s.AdmitAction(tc.req, ReserveNever); got != tc.want {
				t.Fatalf("AdmitAction = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdmitActionReservation pins the reservation rules of ADR 0027's runner
// semantics — which deliveries reserve the turn they are expected to produce,
// and what evidence releases the reservation.
func TestAdmitActionReservation(t *testing.T) {
	// deliver models what the runner does around its single write: admit at
	// the transport boundary, then confirm once the whole payload went out.
	deliver := func(t *testing.T, s *State, req TurnRequirement, pol ReservePolicy) (Admission, bool) {
		t.Helper()
		v, reserved := s.AdmitAction(req, pol)
		if v == Admitted && reserved {
			s.ConfirmDelivery()
		}
		return v, reserved
	}

	t.Run("submit to an idle agent reserves", func(t *testing.T) {
		s := New(Config{ID: "s1"})
		if v, reserved := deliver(t, s, RequireInactive, ReserveIfInactive); v != Admitted || !reserved {
			t.Fatalf("first submit = %v/%v, want Admitted/reserved", v, reserved)
		}
		// The status still says idle — the agent has not reported its turn.
		// A second plain prompt must be refused as a duplicate, distinctly
		// from "the agent is busy".
		if v, _ := deliver(t, s, RequireInactive, ReserveIfInactive); v != RefusedPending {
			t.Fatalf("second submit = %v, want RefusedPending", v)
		}
	})

	t.Run("steer does not reserve a future turn", func(t *testing.T) {
		s := New(Config{ID: "s1"})
		s.SetStatus(&adapter.Status{Active: true})
		if v, reserved := s.AdmitAction(RequireActive, ReserveNever); v != Admitted || reserved {
			t.Fatalf("steer = %v/%v, want Admitted and no reservation", v, reserved)
		}
		// The steered turn ends; the next plain prompt is immediately fine.
		s.CloseTurnFrame(TurnClose{}, &adapter.Status{})
		if v, _ := deliver(t, s, RequireInactive, ReserveIfInactive); v != Admitted {
			t.Fatalf("prompt after a steered turn = %v, want Admitted", v)
		}
	})

	t.Run("send-now while busy does not reserve", func(t *testing.T) {
		s := New(Config{ID: "s1"})
		s.SetStatus(&adapter.Status{Active: true})
		if v, reserved := s.AdmitAction(RequireAny, ReserveIfInactive); v != Admitted || reserved {
			t.Fatalf("require:any send-now while busy = %v/%v, want Admitted, no reservation", v, reserved)
		}
		if s.ReservationHeld() {
			t.Fatal("a submit that joined a running turn must not reserve a future one")
		}
	})

	t.Run("after-turn reserves the queued turn even while busy", func(t *testing.T) {
		s := New(Config{ID: "s1"})
		s.SetStatus(&adapter.Status{Active: true})
		if v, reserved := deliver(t, s, RequireAny, ReserveAlways); v != Admitted || !reserved {
			t.Fatalf("follow-up = %v/%v, want Admitted/reserved", v, reserved)
		}
		// Repeated Active=true reports about the turn that was ALREADY
		// running are not new information (no inactive→active edge), so they
		// must not release the reservation.
		s.SetStatus(&adapter.Status{Active: true})
		s.SetStatus(&adapter.Status{Active: true, Error: true})
		if !s.ReservationHeld() {
			t.Fatal("a repeated Active=true write released the reservation")
		}
		// The CURRENT turn ending is not evidence about the QUEUED prompt
		// either: it will start its own turn afterwards, so a plain prompt now
		// would still be a duplicate.
		s.CloseTurnFrame(TurnClose{}, &adapter.Status{})
		if v, _ := deliver(t, s, RequireInactive, ReserveIfInactive); v != RefusedPending {
			t.Fatalf("prompt after the pre-existing turn ended = %v, want RefusedPending", v)
		}
		// A fresh inactive→active edge IS the evidence.
		s.SetStatus(&adapter.Status{Active: true})
		if s.ReservationHeld() {
			t.Fatal("a fresh active edge must release the reservation")
		}
		s.CloseTurnFrame(TurnClose{}, &adapter.Status{})
		if v, _ := deliver(t, s, RequireInactive, ReserveIfInactive); v != Admitted {
			t.Fatalf("prompt after the queued turn ran = %v, want Admitted", v)
		}
	})

	t.Run("only a fresh active turn releases the reservation", func(t *testing.T) {
		// Every inactive status write is NOT evidence that the delivered
		// prompt was consumed, and must not re-admit a second one: a turn
		// end, a script's PUT /status, a cleared status.
		for _, write := range []func(s *State){
			func(s *State) { s.SetStatus(&adapter.Status{}) },
			func(s *State) { s.SetStatus(&adapter.Status{Error: true}) },
			func(s *State) { s.SetStatus(&adapter.Status{Interrupted: true}) },
			func(s *State) { s.SetStatus(nil) },
			func(s *State) { s.CloseTurnFrame(TurnClose{}, &adapter.Status{}) },
		} {
			s := New(Config{ID: "s1"})
			if _, reserved := deliver(t, s, RequireInactive, ReserveIfInactive); !reserved {
				t.Fatal("setup: expected a reservation")
			}
			write(s)
			if !s.ReservationHeld() {
				t.Fatal("an inactive status write released the reservation")
			}
			if v, _ := deliver(t, s, RequireInactive, ReserveIfInactive); v != RefusedPending {
				t.Fatalf("after an inactive write: %v, want RefusedPending", v)
			}
		}
	})

	t.Run("evidence racing the write is recorded, not consumed", func(t *testing.T) {
		// An agent can start its turn — and its hook can report it — before
		// the runner's write call returns. Such an edge must NOT release the
		// reservation while the write's fate is unknown: if the write then
		// failed, releasing on it would leave the session with neither a
		// delivery nor a reservation, and the next prompt would be admitted
		// as if nothing had been sent.
		s := New(Config{ID: "s1"})
		if _, reserved := s.AdmitAction(RequireInactive, ReserveIfInactive); !reserved {
			t.Fatal("setup: expected a reservation")
		}
		s.SetStatus(&adapter.Status{Active: true}) // edge, in flight
		if !s.ReservationHeld() {
			t.Fatal("an edge released the reservation while the write was still in flight")
		}
		s.ConfirmDelivery()
		if s.ReservationHeld() {
			t.Fatal("the recorded edge was not consumed once the write completed")
		}
	})

	t.Run("evidence racing a FAILED write leaves nothing behind", func(t *testing.T) {
		s := New(Config{ID: "s1"})
		if _, reserved := s.AdmitAction(RequireInactive, ReserveIfInactive); !reserved {
			t.Fatal("setup: expected a reservation")
		}
		s.SetStatus(&adapter.Status{Active: true}) // edge, in flight
		s.ClearReservation()                       // the write failed
		if s.ReservationHeld() {
			t.Fatal("a failed delivery left a reservation behind")
		}
		// And the recorded edge is not carried over to the NEXT delivery.
		s.CloseTurnFrame(TurnClose{}, &adapter.Status{})
		if _, reserved := s.AdmitAction(RequireInactive, ReserveIfInactive); !reserved {
			t.Fatal("expected the retry to reserve")
		}
		s.ConfirmDelivery()
		if !s.ReservationHeld() {
			t.Fatal("the previous delivery's edge was consumed as evidence for the retry")
		}
	})

	t.Run("ClearReservation undoes a failed delivery", func(t *testing.T) {
		s := New(Config{ID: "s1"})
		if _, reserved := deliver(t, s, RequireInactive, ReserveIfInactive); !reserved {
			t.Fatal("setup: expected a reservation")
		}
		s.ClearReservation()
		if v, _ := deliver(t, s, RequireInactive, ReserveIfInactive); v != Admitted {
			t.Fatalf("after ClearReservation: %v, want Admitted", v)
		}
	})
}

// TestAdmitActionIsAtomicAgainstStatusWrites hammers the invariant the whole
// design rests on: a delivered-but-unobserved prompt blocks the next one, and
// only a fresh active turn releases it — so exactly one of several concurrent
// prompts can be admitted per turn.
//
// Each round starts four one-shot admitters behind the same gate. The winner
// confirms its delivery, then the coordinator supplies the sole active edge
// and closes the turn before the next round. An implementation that checks
// activity and reserves in two separate critical sections can admit multiple
// contenders in a round and fails here (and -race flags it as well).
func TestAdmitActionIsAtomicAgainstStatusWrites(t *testing.T) {
	s := New(Config{ID: "s1"})
	const (
		turns      = 500
		contenders = 4
	)

	for turn := range turns {
		start := make(chan struct{})
		results := make(chan bool, contenders)
		var admitters sync.WaitGroup
		for range contenders {
			admitters.Add(1)
			go func() {
				defer admitters.Done()
				<-start
				v, reserved := s.AdmitAction(RequireInactive, ReserveIfInactive)
				if v == Admitted && !reserved {
					t.Error("admitted a plain prompt without reserving the turn it starts")
				}
				if v == Admitted {
					s.ConfirmDelivery() // as the runner does after its write
				}
				results <- v == Admitted
			}()
		}
		close(start)
		admitters.Wait()
		close(results)

		admitted := 0
		for result := range results {
			if result {
				admitted++
			}
		}
		if admitted != 1 {
			t.Fatalf("turn %d: admitted %d of %d concurrent prompts, want 1", turn, admitted, contenders)
		}

		s.SetStatus(&adapter.Status{Active: true}) // the only release
		s.CloseTurnFrame(TurnClose{}, &adapter.Status{})
	}
}
