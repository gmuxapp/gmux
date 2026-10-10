package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// SPIKE measurement harness. Replays a real captured corpus (a read-only
// protocol-3 bootstrap from the operator's daemon, parsed to a rows array)
// through the fanout and reports wire bytes per mutation, per reconnect, ring
// memory and diff CPU.
//
//	GMUX_SPIKE_CORPUS=/tmp/spike/rows.json go test ./cmd/gmuxd -run TestSpikeCorpusMeasurements -v
func TestSpikeCorpusMeasurements(t *testing.T) {
	path := os.Getenv("GMUX_SPIKE_CORPUS")
	if path == "" {
		t.Skip("set GMUX_SPIKE_CORPUS to a rows.json captured from a live bootstrap")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []wire.Session
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	alive := 0
	for _, r := range rows {
		if r.Alive {
			alive++
		}
	}
	t.Logf("corpus: %d rows (%d alive), rows.json %d B", len(rows), alive, len(raw))

	// sseBytes is what the client actually reads off the socket for a set of
	// events: "event: <type>\ndata: <json>\n\n".
	sseBytes := func(events []sessionstream.Event) int {
		total := 0
		for _, e := range events {
			total += len("event: ") + len(e.Type) + 1 + len("data: ") + len(e.Data) + 2
		}
		return total
	}

	clone := func(in []wire.Session) []wire.Session {
		out := make([]wire.Session, len(in))
		copy(out, in)
		return out
	}

	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(func(string) bool { return false })

	broadcast := func(list []wire.Session) (uint64, *sessionEncodeMemo) {
		payload := &wire.SessionsPayload{Sessions: list}
		fanout.BroadcastFrames(wire.Frames{Sessions: payload})
		fanout.mu.Lock()
		epoch := fanout.epoch
		fanout.mu.Unlock()
		return epoch, newSessionEncodeMemo(epoch, payload)
	}

	// Baseline: today's full transaction for the whole corpus.
	epoch0, memo0 := broadcast(clone(rows))
	full, err := memo0.Proto3(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	fullBytes := sseBytes(full)
	t.Logf("BASELINE full transaction: %d events, %d B on the wire", len(full), fullBytes)

	deltaBytesFor := func(from uint64, memo *sessionEncodeMemo) int {
		touched, ok := fanout.TouchedSince(from, memo.epoch, deltaClassAll)
		if !ok {
			t.Fatalf("ring did not cover %d..%d", from, memo.epoch)
		}
		upsert, remove := memo.DeltaRows(false, nil, touched)
		event, fits, err := sessionstream.EncodeDelta(fanout.BootID(), from, memo.epoch, upsert, remove)
		if err != nil {
			t.Fatal(err)
		}
		if !fits {
			t.Logf("  (delta exceeded MaxEventPayload: falls back to the full transaction)")
			return fullBytes
		}
		return sseBytes([]sessionstream.Event{event})
	}

	// --- one mutation: an agent's title changes -----------------------------
	next := clone(rows)
	next[0].Title = next[0].Title + " (edited)"
	epoch1, memo1 := broadcast(next)
	t.Logf("MUTATION title change on 1 row: full %d B -> delta %d B (%.0f\u00d7)",
		fullBytes, deltaBytesFor(epoch0, memo1), float64(fullBytes)/float64(deltaBytesFor(epoch0, memo1)))

	// --- one mutation: status flip on an alive row --------------------------
	next = clone(next)
	for i := range next {
		if next[i].Alive {
			next[i].Status = &wire.Status{Active: !(next[i].Status != nil && next[i].Status.Active)}
			break
		}
	}
	epoch2, memo2 := broadcast(next)
	t.Logf("MUTATION status flip: full %d B -> delta %d B", fullBytes, deltaBytesFor(epoch1, memo2))

	// --- a spawn burst: 4 broadcasts inside one second (#519 window) --------
	burstFrom := epoch2
	burstDelta, burstFull := 0, 0
	cur := clone(next)
	last := epoch2
	for i := 0; i < 4; i++ {
		cur = clone(cur)
		switch i {
		case 0:
			cur = append(cur, wire.Session{ID: "spike-new-session", CreatedAt: time.Now().UTC().Format(time.RFC3339), Alive: true, Adapter: "pi", Command: []string{"pi"}, Cwd: "/home/mg/dev/gmux"})
		case 1:
			cur[len(cur)-1].Title = "a freshly spawned subagent working on something"
		case 2:
			cur[len(cur)-1].Status = &wire.Status{Active: true}
		default:
			cur[len(cur)-1].ParentSessionID = rows[0].ID
		}
		e, m := broadcast(cur)
		burstDelta += deltaBytesFor(last, m)
		burstFull += fullBytes
		last = e
	}
	t.Logf("SPAWN BURST (4 broadcasts): full %d B -> deltas %d B (%.0f\u00d7)", burstFull, burstDelta, float64(burstFull)/float64(burstDelta))

	// --- coalesced burst: one subscriber that only sees the last frame ------
	t.Logf("COALESCED (subscriber that missed 3 of the 4 frames): %d B", deltaBytesFor(burstFrom, mustMemo(t, fanout, cur)))

	// --- reconnect after a gap ---------------------------------------------
	// Steady state on the real corpus is ~13 broadcasts / 25 min (0.52/min).
	for _, gap := range []struct {
		label     string
		mutations int
	}{{"30 s (0-1 mutations)", 1}, {"5 min (~3 mutations)", 3}, {"30 min (~16 mutations)", 16}, {"2 h (~62 mutations)", 62}} {
		resumeFrom := last
		list := clone(cur)
		for i := 0; i < gap.mutations; i++ {
			list = clone(list)
			list[i%len(list)].Title = fmt.Sprintf("churn-%d", i)
			list[(i*7)%len(list)].LastOutputAt = time.Now().UTC().Format(time.RFC3339)
			e, _ := broadcast(list)
			last = e
		}
		cur = list
		t.Logf("RECONNECT after %s: bootstrap %d B -> resume %d B", gap.label, fullBytes, deltaBytesFor(resumeFrom, mustMemo(t, fanout, cur)))
	}

	// --- ring memory at 1,000 epochs ---------------------------------------
	ringFanout := newSSEFanout()
	ringFanout.SetOwnershipFilter(func(string) bool { return false })
	list := clone(rows)
	for i := 0; i < 1000; i++ {
		list = clone(list)
		list[i%len(list)].Title = fmt.Sprintf("churn-%d", i)
		ringFanout.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: list}})
	}
	ringFanout.mu.Lock()
	entries, ids, bytes := len(ringFanout.ring.entries), ringFanout.ring.ids, ringFanout.ring.approxBytes()
	ringFanout.mu.Unlock()
	t.Logf("RING after 1000 single-row epochs: %d entries, %d retained ids, ~%d B (incl. the O(N) prev-hash/membership tables)", entries, ids, bytes)

	// worst case: every epoch touches every row
	ringFanout2 := newSSEFanout()
	ringFanout2.SetOwnershipFilter(func(string) bool { return false })
	for i := 0; i < 200; i++ {
		l := clone(rows)
		for j := range l {
			l[j].LastOutputAt = fmt.Sprintf("2026-09-14T13:%02d:%02dZ", i%60, j%60)
		}
		ringFanout2.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l}})
	}
	ringFanout2.mu.Lock()
	entries2, ids2, bytes2 := len(ringFanout2.ring.entries), ringFanout2.ring.ids, ringFanout2.ring.approxBytes()
	base2 := ringFanout2.ring.base
	ringFanout2.mu.Unlock()
	t.Logf("RING worst case (every epoch touches all %d rows): %d entries kept, %d ids, ~%d B, base advanced to epoch %d", len(rows), entries2, ids2, bytes2, base2)

	// --- diff CPU per broadcast --------------------------------------------
	cpuFanout := newSSEFanout()
	cpuFanout.SetOwnershipFilter(func(string) bool { return false })
	l := clone(rows)
	cpuFanout.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l}})
	runtime.GC()
	start := time.Now()
	const iters = 20
	for i := 0; i < iters; i++ {
		l2 := clone(l)
		l2[i].Title = fmt.Sprintf("cpu-%d", i)
		cpuFanout.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l2}})
	}
	perBroadcast := time.Since(start) / iters
	startEnc := time.Now()
	for i := 0; i < iters; i++ {
		m := newSessionEncodeMemo(uint64(i+1), &wire.SessionsPayload{Sessions: l})
		if _, err := m.Proto3(false, nil); err != nil {
			t.Fatal(err)
		}
	}
	perEncode := time.Since(startEnc) / iters

	control := newSSEFanout()
	control.DisableDeltas()
	control.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l}})
	runtime.GC()
	startCtl := time.Now()
	for i := 0; i < iters; i++ {
		l2 := clone(l)
		l2[i].Title = fmt.Sprintf("cpu-%d", i)
		control.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l2}})
	}
	perControl := time.Since(startCtl) / iters
	t.Logf("CPU at N=%d: BroadcastFrames today %v -> with ring %v (diff cost %v); full transaction encode %v/broadcast (skipped for delta subscribers)",
		len(rows), perControl, perBroadcast, perBroadcast-perControl, perEncode)

	// ring memory breakdown
	bare := newSSEFanout()
	bare.SetOwnershipFilter(func(string) bool { return false })
	bare.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: clone(rows)}})
	bare.mu.Lock()
	baseTables := bare.ring.approxBytes()
	bare.mu.Unlock()
	t.Logf("RING breakdown: O(N) prev-hash + membership tables ~%d B at N=%d; per-epoch entries are the remainder", baseTables, len(rows))
}

func mustMemo(t *testing.T, f *sseFanout, list []wire.Session) *sessionEncodeMemo {
	t.Helper()
	f.mu.Lock()
	epoch := f.epoch
	f.mu.Unlock()
	return newSessionEncodeMemo(epoch, &wire.SessionsPayload{Sessions: list})
}
