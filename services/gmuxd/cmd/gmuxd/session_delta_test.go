package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// SPIKE oracle: for every pair of epochs (i, j) in a randomized broadcast
// sequence, a subscriber that holds snapshot i and applies the delta the
// server would send at epoch j must end up with a map deep-equal to snapshot
// j — for BOTH audience classes. This is the property the whole design rests
// on ("replay of v3+delta == v3").
func TestDeltaReplayEqualsSnapshots(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	isLocalPeer := func(name string) bool { return name == "box" }

	peers := []string{"", "", "", "box", "remote"}
	makeRow := func(i, rev int) wire.Session {
		return wire.Session{
			ID:    fmt.Sprintf("s%02d", i),
			Peer:  peers[i%len(peers)],
			Title: fmt.Sprintf("title-%d", rev),
			Alive: rev%2 == 0,
		}
	}

	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(isLocalPeer)

	const rows = 40
	revs := make([]int, rows)
	live := map[int]bool{}
	for i := 0; i < rows; i++ {
		live[i] = true
	}
	type step struct {
		epoch uint64
		memo  *sessionEncodeMemo
		snap  map[bool]map[string]string // class -> id -> encoded row
	}
	var steps []step

	for n := 0; n < 60; n++ {
		// mutate: bump revisions, drop rows, re-add rows, flip a peer
		switch rng.Intn(4) {
		case 0:
			revs[rng.Intn(rows)]++
		case 1:
			delete(live, rng.Intn(rows))
		case 2:
			live[rng.Intn(rows)] = true
		case 3:
			revs[rng.Intn(rows)] += 2
		}
		var list []wire.Session
		for i := 0; i < rows; i++ {
			if live[i] {
				list = append(list, makeRow(i, revs[i]))
			}
		}
		payload := &wire.SessionsPayload{Sessions: list}
		fanout.BroadcastFrames(wire.Frames{Sessions: payload})
		fanout.mu.Lock()
		epoch := fanout.epoch
		fanout.mu.Unlock()
		memo := newSessionEncodeMemo(epoch, payload)
		snap := map[bool]map[string]string{}
		for _, class := range []bool{false, true} {
			m := map[string]string{}
			p := payload
			if class {
				f := payload.FilterOwned(isLocalPeer)
				p = &f
			}
			for _, s := range p.Sessions {
				b, _ := json.Marshal(s)
				m[s.ID] = string(b)
			}
			snap[class] = m
		}
		steps = append(steps, step{epoch: epoch, memo: memo, snap: snap})
	}

	for i := range steps {
		for j := i + 1; j < len(steps); j++ {
			for _, class := range []bool{false, true} {
				cls := deltaClassAll
				if class {
					cls = deltaClassOwned
				}
				touched, ok := fanout.TouchedSince(steps[i].epoch, steps[j].epoch, cls)
				if !ok {
					t.Fatalf("chain %d->%d class=%v not covered by the ring", i, j, class)
				}
				upsert, remove := steps[j].memo.DeltaRows(class, isLocalPeer, touched)
				got := map[string]string{}
				for id, row := range steps[i].snap[class] {
					got[id] = row
				}
				for _, id := range remove {
					delete(got, id)
				}
				for _, row := range upsert {
					b, _ := json.Marshal(row)
					got[row.ID] = string(b)
				}
				want := steps[j].snap[class]
				if len(got) != len(want) {
					t.Fatalf("chain %d->%d class=%v: %d rows, want %d", i, j, class, len(got), len(want))
				}
				for id, row := range want {
					if got[id] != row {
						t.Fatalf("chain %d->%d class=%v: row %s diverged\n got=%s\nwant=%s", i, j, class, id, got[id], row)
					}
				}
			}
		}
	}
}

// A row that leaves the ?as=peer audience because its owner stopped being a
// Local peer must reach that audience as a removal. Ownership is not part of
// the row's bytes, so this is the case a content-only diff would miss.
func TestDeltaOwnershipFlipLooksLikeRemoval(t *testing.T) {
	localPeers := map[string]bool{"box": true}
	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(func(name string) bool { return localPeers[name] })

	rowsOf := func() *wire.SessionsPayload {
		return &wire.SessionsPayload{Sessions: []wire.Session{
			{ID: "local-1"},
			{ID: "box/remote-1", Peer: "box"},
		}}
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: rowsOf()})
	fanout.mu.Lock()
	first := fanout.epoch
	fanout.mu.Unlock()

	localPeers["box"] = false // peer reclassified; rows unchanged byte-wise
	payload := rowsOf()
	fanout.BroadcastFrames(wire.Frames{Sessions: payload})
	fanout.mu.Lock()
	second := fanout.epoch
	fanout.mu.Unlock()

	touched, ok := fanout.TouchedSince(first, second, deltaClassOwned)
	if !ok {
		t.Fatal("ring did not cover the flip")
	}
	memo := newSessionEncodeMemo(second, payload)
	upsert, remove := memo.DeltaRows(true, func(name string) bool { return localPeers[name] }, touched)
	if len(upsert) != 0 || len(remove) != 1 || remove[0] != "box/remote-1" {
		t.Fatalf("ownership flip: upsert=%v remove=%v, want a single removal of box/remote-1", upsert, remove)
	}
	// The browser audience keeps the row: it is filtered differently.
	touchedAll, _ := fanout.TouchedSince(first, second, deltaClassAll)
	if len(touchedAll) != 0 {
		t.Fatalf("browser audience saw %v changed, want nothing", touchedAll)
	}
}

// Eviction is the honest failure mode: a subscriber older than the ring base
// is told "no chain", and the caller falls back to today's full bootstrap.
func TestDeltaRingEvictionRefusesStaleResume(t *testing.T) {
	fanout := newSSEFanout()
	fanout.ring.maxEntries = 4
	for i := 0; i < 10; i++ {
		fanout.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "a", Title: fmt.Sprint(i)}}}})
	}
	fanout.mu.Lock()
	latest := fanout.epoch
	fanout.mu.Unlock()
	if _, ok := fanout.TouchedSince(1, latest, deltaClassAll); ok {
		t.Fatal("resume from an evicted epoch must be refused")
	}
	if _, ok := fanout.TouchedSince(latest-1, latest, deltaClassAll); !ok {
		t.Fatal("resume from the newest epoch must be honored")
	}
}

// Oversized deltas fall back rather than splitting: the delta event is the
// unit of atomicity, so it must fit one SSE payload.
func TestDeltaOversizedFallsBack(t *testing.T) {
	big := make([]wire.Session, 200)
	for i := range big {
		big[i] = wire.Session{ID: fmt.Sprintf("s%03d", i), Title: string(make([]byte, 400))}
	}
	_, fits, err := sessionstream.EncodeDelta("boot", 1, 2, big, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fits {
		t.Fatal("a delta above MaxEventPayload must report !fits so the sender falls back")
	}
	_, fits, err = sessionstream.EncodeDelta("boot", 1, 2, big[:2], []string{"x"})
	if err != nil || !fits {
		t.Fatalf("small delta: fits=%v err=%v", fits, err)
	}
}
