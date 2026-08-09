package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// Subscribe captures the baseline and installs the subscriber under the same
// mutex BroadcastFrames uses. This deterministic boundary is what lets the
// handler finish baseline epoch N and then deliver a concurrently committed
// replacement as epoch N+1 without a lost mutation.
func TestSessionStreamVersionCompatibility(t *testing.T) {
	if !useSemanticSessionStream(false, "") {
		t.Fatal("version-locked browser must receive protocol 3")
	}
	if useSemanticSessionStream(true, "") {
		t.Fatal("old peer must receive legacy fallback")
	}
	if !useSemanticSessionStream(true, "3") {
		t.Fatal("new peer did not opt into protocol 3")
	}
	if useSemanticSessionStream(true, "99") {
		t.Fatal("unknown peer protocol must not be guessed")
	}
}

func TestThousandSessionWorldIsSeparateAndBounded(t *testing.T) {
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x", i)
	}
	world := wire.WorldPayload{Projects: []wire.ProjectItem{{Slug: "large", Sessions: ids}}}
	data, err := json.Marshal(world)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > sessionstream.MaxEventPayload {
		t.Fatalf("snapshot.world payload=%d exceeds session event budget=%d", len(data), sessionstream.MaxEventPayload)
	}
	if string(data) == "" {
		t.Fatal("empty world encoding")
	}
	t.Logf("1000-session snapshot.world payload=%d (membership IDs only; no session rows)", len(data))
}

func TestFanoutSubscribeSnapshotBoundary(t *testing.T) {
	f := newSSEFanout()
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "before"}}}})
	initial, ch, cancel := f.Subscribe()
	defer cancel()
	if got := initial.Sessions.Sessions[0].ID; got != "before" {
		t.Fatalf("initial=%q", got)
	}

	done := make(chan struct{})
	go func() {
		f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "after"}}}})
		close(done)
	}()
	select {
	case msg := <-ch:
		if got := msg.Frames.Sessions.Sessions[0].ID; got != "after" {
			t.Fatalf("queued=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation after subscribe was lost")
	}
	<-done
}
