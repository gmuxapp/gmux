package session

import (
	"fmt"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestExchangeFrameSpansInstructionsAndIterations(t *testing.T) {
	s := New(Config{ID: "id", Adapter: "pi"})
	s.OpenTurn(7, "first", 0)
	s.NoteIteration(7)
	s.NoteIteration(7)
	s.NoteInjection(7, " follow\nup ", 0)
	s.NoteIteration(7)
	s.CloseTurnFrame(TurnClose{TurnSeq: 7, Outcome: "completed", Output: "done"}, &adapter.Status{})
	f := s.TurnFrameSnapshot()
	if f == nil || f.Last == nil {
		t.Fatal("missing close")
	}
	if len(f.Last.Exchanges) != 2 || f.Last.Exchanges[0].Iterations != 2 || f.Last.Exchanges[1].Iterations != 1 || f.Last.Exchanges[1].User != " follow\nup " {
		t.Fatalf("%+v", f.Last.Exchanges)
	}
}

func TestExchangeFrameBoundsAndReportsOmission(t *testing.T) {
	s := New(Config{ID: "id", Adapter: "pi"})
	s.OpenTurn(1, "anchor", 1<<20)
	for i := 0; i < maxLiveInstructions+5; i++ {
		s.NoteInjection(1, fmt.Sprintf("message-%d", i), len(fmt.Sprintf("message-%d", i)))
	}
	cur := s.TurnFrameSnapshot().Current
	if len(cur.Exchanges) != maxLiveInstructions+1 {
		t.Fatalf("len=%d", len(cur.Exchanges))
	}
	if cur.OmittedExchanges != 5 || cur.OmittedBytes < 1<<20 {
		t.Fatalf("source omission=%d/%d", cur.OmittedExchanges, cur.OmittedBytes)
	}
	if got := cur.Exchanges[len(cur.Exchanges)-1].Ordinal; got != maxLiveInstructions+6 {
		t.Fatalf("ordinal drifted after eviction: %d", got)
	}
}
