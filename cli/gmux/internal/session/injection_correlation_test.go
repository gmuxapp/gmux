package session

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// The correlation contract (ADR 0027's steer self-exclusion): the runner can say
// which injection is the one a given gmux request delivered, and it says so from
// the adapter's own report rather than from delivery order.
func TestInjectionCorrelation(t *testing.T) {
	open := func() *State {
		s := New(Config{ID: "s1", Adapter: "pi"})
		s.OpenTurn(1, "go")
		return s
	}
	lastInjection := func(t *testing.T, s *State) TurnInjection {
		t.Helper()
		inj := s.TurnFrameSnapshot().Current.Injections
		if len(inj) == 0 {
			t.Fatal("no injection was recorded")
		}
		return inj[len(inj)-1]
	}

	t.Run("the delivered text carries the delivering request's id", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "use the other API")
		s.NoteInjection(1, "use the other API", false)
		if got := lastInjection(t, s); got.DeliveryID != "d-1" {
			t.Fatalf("injection=%+v", got)
		}
	})

	// The reviewers' probes: an uncapped report must match EXACTLY, so no
	// shorter message can consume a longer delivery's identity. Both cases used
	// to hand the caller an id it had not earned, and the caller — "acknowledged
	// and last" — was then served the merged close under exit 0, which is
	// exactly the settle-before-ack case that must read as indeterminate.
	t.Run("a shorter human message cannot steal a pending id", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "stop and say kiwi")
		s.NoteInjection(1, "stop", false) // typed into the TUI
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a prefix of a pending delivery claimed its identity: %+v", got)
		}
		// And the delivery is still armed for its own message.
		s.NoteInjection(1, "stop and say kiwi", false)
		if got := lastInjection(t, s); got.DeliveryID != "id-A" {
			t.Fatalf("the real injection lost its identity: %+v", got)
		}
	})

	t.Run("a shorter caller's steer cannot steal another caller's id", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "stop and say kiwi")
		s.NotePendingInjection("id-B", "stop")
		s.NoteInjection(1, "stop", false)
		if got := lastInjection(t, s); got.DeliveryID != "id-B" {
			t.Fatalf("the wrong caller was credited with the injection: %+v", got)
		}
	})

	t.Run("identical pending texts are attributed to nobody", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "same")
		s.NotePendingInjection("id-B", "same")
		s.NoteInjection(1, "same", false)
		// No fact in the report separates them, so gmux declines to guess: both
		// injectors resolve indeterminate and every bystander is interrupted by
		// an id-less injection. An arbitrary first match would be a coin flip on
		// whose answer the close is.
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("an ambiguous report was attributed to %q", got.DeliveryID)
		}
	})

	t.Run("an ambiguous capped excerpt is attributed to nobody", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "please switch to the streaming endpoint")
		s.NotePendingInjection("id-B", "please switch to the batch endpoint")
		s.NoteInjection(1, "please switch to the\u2026", true)
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a truncated excerpt matching two deliveries was attributed to %q", got.DeliveryID)
		}
	})

	t.Run("an unambiguous capped excerpt still matches", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "please switch to the streaming endpoint")
		s.NotePendingInjection("id-B", "and skip the migration")
		s.NoteInjection(1, "please switch to the\u2026", true)
		if got := lastInjection(t, s); got.DeliveryID != "id-A" {
			t.Fatalf("injection=%+v", got)
		}
	})

	t.Run("a prefix the adapter did not flag as capped is not a match", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "please switch to the streaming endpoint")
		// The adapter reports truncation as a fact; without it, a prefix is
		// somebody else's shorter message.
		s.NoteInjection(1, "please switch to the", false)
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("an unflagged prefix matched: %+v", got)
		}
	})

	// The delta's D1 repro: a message that genuinely ENDS IN an ellipsis and
	// happens to prefix an in-flight delivery. Reading the marker off the text
	// could not tell it from a capped excerpt, so it bought itself the prefix
	// rule and stole the delivery's identity — leaving that caller "acknowledged
	// and last" and served the merged close under exit 0. The flag is the
	// adapter's own assertion and this text does not carry it.
	t.Run("a foreign message ending in an ellipsis cannot buy the prefix rule", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "wait\u2026 actually use the other API")
		s.NoteInjection(1, "wait\u2026", false) // typed by a human, not capped by the adapter
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a message ending in an ellipsis claimed a pending delivery: %+v", got)
		}
		// The delivery is still armed for its own, whole message.
		s.NoteInjection(1, "wait\u2026 actually use the other API", false)
		if got := lastInjection(t, s); got.DeliveryID != "id-A" {
			t.Fatalf("the real injection lost its identity: %+v", got)
		}
	})

	// ...and the mirror image: a delivery whose text ends in an ellipsis is
	// matched exactly, with no trimming, because the marker is part of the
	// message when the adapter did not flag a cap.
	t.Run("an unflagged report keeps its ellipsis for the comparison", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "hold on\u2026")
		s.NoteInjection(1, "hold on\u2026", false)
		if got := lastInjection(t, s); got.DeliveryID != "id-A" {
			t.Fatalf("an exact match was trimmed apart: %+v", got)
		}
	})

	t.Run("normalization spans both runtimes' whitespace", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("id-A", "bom\uFEFFhere and nel\u0085there")
		// What pi-ext.mjs's excerpt() produces for that text: both sides collapse
		// the union of the two runtimes' whitespace classes.
		s.NoteInjection(1, "bom here and nel there", false)
		if got := lastInjection(t, s); got.DeliveryID != "id-A" {
			t.Fatalf("normalization diverged between the adapter and the runner: %+v", got)
		}
	})

	t.Run("an excerpted report still matches its delivery", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "please switch to the streaming endpoint and retry")
		// The adapter caps excerpts at the source and SAYS SO on the event, so the
		// report is known to be a prefix of what was delivered.
		s.NoteInjection(1, "please switch to the streaming\u2026", true)
		if got := lastInjection(t, s); got.DeliveryID != "d-1" {
			t.Fatalf("a capped excerpt lost its correlation: %+v", got)
		}
	})

	t.Run("whitespace differences do not break the match", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "line one\n\nline two")
		s.NoteInjection(1, "line one line two", false)
		if got := lastInjection(t, s); got.DeliveryID != "d-1" {
			t.Fatalf("injection=%+v", got)
		}
	})

	t.Run("a human's message never inherits a pending id", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "use the other API")
		// A human typed something else into the TUI while the steer was in
		// flight. Matching by arrival order would hand this message the caller's
		// identity, and the caller would then claim an answer its own text never
		// entered.
		s.NoteInjection(1, "stop, I'll drive", false)
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a foreign injection was attributed to a pending delivery: %+v", got)
		}
		// The pending record survives for its own message.
		s.NoteInjection(1, "use the other API", false)
		if got := lastInjection(t, s); got.DeliveryID != "d-1" {
			t.Fatalf("injection=%+v", got)
		}
	})

	t.Run("one delivery is claimed once", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "again")
		s.NoteInjection(1, "again", false)
		s.NoteInjection(1, "again", false)
		inj := s.TurnFrameSnapshot().Current.Injections
		if len(inj) != 2 || inj[0].DeliveryID != "d-1" || inj[1].DeliveryID != "" {
			t.Fatalf("a consumed delivery id was reused: %+v", inj)
		}
	})

	t.Run("a withdrawn delivery matches nothing", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "half-written")
		s.DropPendingInjection("d-1")
		s.NoteInjection(1, "half-written", false)
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a failed write still claimed an injection: %+v", got)
		}
	})

	t.Run("a delivery that STARTED a turn is that turn's trigger", func(t *testing.T) {
		s := New(Config{ID: "s1", Adapter: "pi"})
		s.NotePendingInjection("d-1", "first ask")
		s.OpenTurn(1, "first ask")
		// pi replays the opening prompt as a message on the loop; the runner's
		// window is already closed, so nothing can mistake it for an injection
		// belonging to a wait.
		s.NoteInjection(1, "first ask", false)
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a trigger was correlated as an injection: %+v", got)
		}
	})

	t.Run("the window does not outlive the turn", func(t *testing.T) {
		s := open()
		s.NotePendingInjection("d-1", "too late")
		s.CloseTurnFrame(TurnClose{TurnSeq: 1, Outcome: "completed"}, &adapter.Status{})
		s.OpenTurn(2, "next")
		s.NoteInjection(2, "too late", false)
		if got := lastInjection(t, s); got.DeliveryID != "" {
			t.Fatalf("a stale delivery claimed the NEXT turn's injection: %+v", got)
		}
	})

	t.Run("the pending window is bounded", func(t *testing.T) {
		s := open()
		for i := 0; i < maxPendingInjections*3; i++ {
			s.NotePendingInjection("d", "text")
		}
		if got := len(s.pendingInjections); got > maxPendingInjections {
			t.Fatalf("pending window grew to %d", got)
		}
	})
}

// The frame is a wire contract between the runner and gmuxd, and the runner may
// be older than the daemon reading it: the object shape and the bare excerpt
// string an earlier runner sent must both decode, because a frame that fails to
// decode wholesale costs every result in it.
func TestTurnInjectionDecodesBothShapes(t *testing.T) {
	var frame TurnFrame
	raw := `{"seq":1,"current":{"turn_seq":3,"injections":["old shape",{"text":"new shape","delivery_id":"d-9"}]}}`
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	inj := frame.Current.Injections
	if len(inj) != 2 || inj[0] != (TurnInjection{Text: "old shape"}) ||
		inj[1] != (TurnInjection{Text: "new shape", DeliveryID: "d-9"}) {
		t.Fatalf("injections=%+v", inj)
	}
}

// The injection COUNT is what novelty is decided against downstream, so it must
// keep growing after the bounded excerpt list has saturated. A count that
// saturated with the list would make every later injection look like "nothing
// new" to a wait that armed at that point — and that wait would then be served
// the merged answer under exit 0, which is the one direction the steer rule may
// not fail in.
func TestInjectionCountOutlivesTheBoundedList(t *testing.T) {
	s := New(Config{ID: "s1", Adapter: "pi"})
	s.OpenTurn(1, "go")
	const total = maxTurnInjections * 2
	for i := 0; i < total; i++ {
		s.NoteInjection(1, "steer "+strconv.Itoa(i), false)
	}
	cur := s.TurnFrameSnapshot().Current
	if len(cur.Injections) != maxTurnInjections {
		t.Fatalf("retained %d injections, want the list capped at %d", len(cur.Injections), maxTurnInjections)
	}
	if cur.InjectionCount != total {
		t.Fatalf("InjectionCount = %d after %d injections", cur.InjectionCount, total)
	}
	// The newest is the one that owns the answer, and it is the one kept.
	if last := cur.Injections[len(cur.Injections)-1]; last.Text != "steer "+strconv.Itoa(total-1) {
		t.Fatalf("the bounded list dropped the NEWEST injection: %+v", last)
	}
	// The close carries the count too, so a waiter that only learns of a steer
	// at the close decides novelty exactly as it would have live.
	s.CloseTurnFrame(TurnClose{TurnSeq: 1, Outcome: "completed"}, &adapter.Status{})
	if got := s.TurnFrameSnapshot().Last.InjectionCount; got != total {
		t.Fatalf("close carried InjectionCount = %d, want %d", got, total)
	}
}
