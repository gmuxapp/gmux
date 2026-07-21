package main

import (
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// The world frame is only recomposed on project/peer batches, so the health
// counts embedded in it go stale across liveness-only changes (a runner
// registering or dying never refreshes them). /v1/health and the SSE initial
// snapshot therefore derive counts from the sessions frame at read time.
// This pins the derivation; the live symptom was /v1/health reporting
// local_alive=0 while /v1/sessions listed an alive session.
func TestFreshHealthCountsDerivesFromSessionsFrame(t *testing.T) {
	frames := wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{
		{ID: "a", Alive: true},
		{ID: "b", Alive: true},
		{ID: "c", Alive: false},
		{ID: "d", Alive: true, Peer: "spoke"},
		{ID: "e", Alive: false, Peer: "spoke"},
	}}}
	counts, ok := freshHealthCounts(frames)
	if !ok {
		t.Fatal("expected ok with a sessions frame present")
	}
	if counts.LocalAlive != 2 || counts.RemoteAlive != 1 || counts.Dead != 2 {
		t.Fatalf("counts = %+v, want local_alive=2 remote_alive=1 dead=2", counts)
	}

	if _, ok := freshHealthCounts(wire.Frames{}); ok {
		t.Fatal("no sessions frame must report !ok so callers keep the composed counts")
	}
}
