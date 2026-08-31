package main

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// One broadcast must be encoded at most once per (filter class × protocol),
// and every subscriber of a class must receive the identical bytes the
// per-subscriber encode used to produce.
func TestSessionEncodeMemoSharesIdenticalFrames(t *testing.T) {
	payload := &wire.SessionsPayload{Sessions: []wire.Session{
		{ID: "own", Adapter: "shell"},
		{ID: "dc@devbox", Peer: "devbox", Adapter: "shell"},
		{ID: "mirror@remote", Peer: "remote", Adapter: "shell"},
	}}
	isLocalPeer := func(name string) bool { return name == "devbox" }
	memo := newSessionEncodeMemo(7, payload)

	full, err := memo.Proto2(false, isLocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(payload)
	if !bytes.Equal(full, want) {
		t.Fatalf("proto2 browser bytes diverge from direct marshal:\n%s\n%s", full, want)
	}
	again, _ := memo.Proto2(false, isLocalPeer)
	if &full[0] != &again[0] {
		t.Fatal("proto2 browser frame re-encoded instead of shared")
	}

	peer, err := memo.Proto2(true, isLocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	var filtered wire.SessionsPayload
	if err := json.Unmarshal(peer, &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Sessions) != 2 || filtered.Sessions[0].ID != "own" || filtered.Sessions[1].ID != "dc@devbox" {
		t.Fatalf("peer class must keep own + Local-peer rows only: %s", peer)
	}

	e1, err := memo.Proto3(false, isLocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := memo.Proto3(false, isLocalPeer)
	if len(e1) == 0 || len(e1) != len(e2) || &e1[0].Data[0] != &e2[0].Data[0] {
		t.Fatal("proto3 events re-encoded instead of shared")
	}
	if !bytes.Contains(e1[0].Data, []byte(`"epoch":7`)) {
		t.Fatalf("proto3 begin must carry the broadcast epoch: %s", e1[0].Data)
	}
	p3peer, err := memo.Proto3(true, isLocalPeer)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.Join([][]byte{p3peer[0].Data, p3peer[1].Data}, nil), []byte("mirror@remote")) {
		t.Fatal("proto3 peer class leaked a network-peer mirror row")
	}
}

// Epochs are drawn under the fanout mutex at broadcast/subscribe time, so
// each connection observes strictly increasing epochs in delivery order —
// even when a later broadcast is encoded first by another subscriber.
func TestFanoutEpochStrictlyIncreasingPerSubscriber(t *testing.T) {
	f := newSSEFanout()
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "seed"}}}})
	initial, ch, cancel := f.Subscribe()
	defer cancel()
	if initial.SessionsEncode == nil {
		t.Fatal("initial snapshot must carry an encode memo")
	}
	last := initial.SessionsEncode.epoch
	for i := 0; i < 3; i++ {
		f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "seed"}}}})
		msg := <-ch
		if msg.SessionsEncode == nil {
			t.Fatal("broadcast must carry an encode memo")
		}
		// Encode lazily *after* a later broadcast already exists elsewhere:
		// epoch order must still match delivery order.
		if msg.SessionsEncode.epoch <= last {
			t.Fatalf("epoch not strictly increasing: %d after %d", msg.SessionsEncode.epoch, last)
		}
		last = msg.SessionsEncode.epoch
	}
}

// Concurrent subscribers racing to encode the same broadcast must all get a
// consistent result (exercised with -race in CI).
func TestSessionEncodeMemoConcurrentEncode(t *testing.T) {
	payload := realisticSessionsPayload(200)
	memo := newSessionEncodeMemo(1, payload)
	isLocalPeer := func(string) bool { return false }
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		peer := i%2 == 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := memo.Proto2(peer, isLocalPeer); err != nil {
				errs <- err
			}
			if _, err := memo.Proto3(peer, isLocalPeer); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
