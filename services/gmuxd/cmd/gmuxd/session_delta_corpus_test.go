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

// PROTO measurement harness. Replays a real captured corpus (a read-only
// protocol-3 bootstrap from the operator's daemon, parsed to a rows array)
// through the fanout and reports the numbers the world-split report needs.
//
//	GMUX_SPIKE_CORPUS=/tmp/pws/rows.json go test ./cmd/gmuxd -run TestSpikeCorpusMeasurements -v
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
	findRoot := func(list []wire.Session, title string, peer string) int {
		for i, s := range list {
			if s.Title == title && s.Peer == peer && s.ParentSessionID == "" {
				return i
			}
		}
		return -1
	}

	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(func(string) bool { return false })
	fanout.DemandClass(deltaClassRoots)
	fanout.DemandClass(deltaClassAll)
	conn, release := fanout.RegisterConn()
	defer release()

	broadcast := func(list []wire.Session) (uint64, *sessionEncodeMemo) {
		fanout.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: list}})
		memo, epoch := fanout.CurrentMemo()
		return epoch, memo
	}

	// --- bootstrap: full vs roots -----------------------------------------
	epoch0, memo0 := broadcast(clone(rows))
	full, _ := memo0.Proto3(deltaClassAll)
	rootsTx, _ := memo0.Proto3(deltaClassRoots)
	fullBytes, rootsBytes := sseBytes(full), sseBytes(rootsTx)
	rootRows := memo0.ViewRows(deltaClassRoots)
	t.Logf("BOOTSTRAP full: %d rows, %d events, %d B", len(rows), len(full), fullBytes)
	t.Logf("BOOTSTRAP roots: %d rows, %d events, %d B (%.1fx smaller)", len(rootRows), len(rootsTx), rootsBytes, float64(fullBytes)/float64(rootsBytes))
	withCounts := 0
	for _, r := range rootRows {
		if r.DescendantCounts != nil {
			withCounts++
		}
	}
	t.Logf("  roots carrying descendant_counts: %d", withCounts)

	deltaBytes := func(from uint64, memo *sessionEncodeMemo, class int) (int, int) {
		touched, ok := fanout.TouchedSince(from, memo.epoch, class)
		if !ok {
			t.Fatalf("ring did not cover %d..%d", from, memo.epoch)
		}
		upsert, remove := memo.DeltaRows(class, touched)
		event, fits, err := sessionstream.EncodeDelta(fanout.BootID(), from, memo.epoch, upsert, remove)
		if err != nil {
			t.Fatal(err)
		}
		if !fits {
			if class == deltaClassRoots {
				return rootsBytes, len(touched)
			}
			return fullBytes, len(touched)
		}
		return sseBytes([]sessionstream.Event{event}), len(touched)
	}

	// --- one mutation on a ROOT (title) -----------------------------------
	driver := findRoot(rows, "DRIVER", "")
	club := findRoot(rows, "club-3090", "gmux-hs")
	if driver < 0 || club < 0 {
		t.Fatalf("corpus lacks DRIVER (%d) or club-3090 (%d)", driver, club)
	}
	next := clone(rows)
	next[driver].Title = "DRIVER (edited)"
	epoch1, memo1 := broadcast(next)
	fb, _ := deltaBytes(epoch0, memo1, deltaClassAll)
	rb, _ := deltaBytes(epoch0, memo1, deltaClassRoots)
	t.Logf("MUTATION root title: full-tx %d B | full-delta %d B | roots-delta %d B", fullBytes, fb, rb)

	// --- one mutation on a CHILD of DRIVER (status flip) — drawer closed ----
	childOf := func(list []wire.Session, parentID string) int {
		for i, s := range list {
			if s.ParentSessionID == parentID {
				return i
			}
		}
		return -1
	}
	kid := childOf(next, next[driver].ID)
	next = clone(next)
	next[kid].Status = &wire.Status{Active: !(next[kid].Status != nil && next[kid].Status.Active)}
	next[kid].Alive = true
	epoch2, memo2 := broadcast(next)
	fb, _ = deltaBytes(epoch1, memo2, deltaClassAll)
	rb, touchedN := deltaBytes(epoch1, memo2, deltaClassRoots)
	t.Logf("MUTATION child status (drawer closed): full-tx %d B | full-delta %d B | roots-delta %d B (%d root(s) touched: counts)", fullBytes, fb, rb, touchedN)

	// --- burst of 8 child mutations under DRIVER, drawer CLOSED ------------
	burst := func(label string, from []wire.Session, fromEpoch uint64, scoped bool) (rootsSum, scopeSum, events int, last uint64, out []wire.Session) {
		cur := from
		last = fromEpoch
		for i := 0; i < 8; i++ {
			cur = clone(cur)
			switch i % 4 {
			case 0:
				cur = append(cur, wire.Session{ID: fmt.Sprintf("burst-%s-%d", label, i), CreatedAt: time.Now().UTC().Format(time.RFC3339), Alive: true, Adapter: "pi", SemanticAgent: true, ParentSessionID: rows[driver].ID, Command: []string{"pi"}, Cwd: "/home/mg/dev/gmux"})
			case 1:
				cur[len(cur)-1].Title = "a freshly spawned subagent working on something"
			case 2:
				cur[len(cur)-1].Status = &wire.Status{Active: true}
			default:
				cur[len(cur)-1].Status = &wire.Status{Active: false}
				cur[len(cur)-1].Unread = true
			}
			e, m := broadcast(cur)
			b, _ := deltaBytes(last, m, deltaClassRoots)
			rootsSum += b
			events++
			if scoped {
				key := scopeChildrenPrefix + rows[driver].ID
				touched, ok := fanout.ScopeTouchedSince(last, e, key)
				if !ok {
					t.Fatalf("scope chain broke at %d", e)
				}
				up, rm := m.ScopeDeltaRows(key, touched)
				ev, fits, _ := sessionstream.EncodeScopeDelta(fanout.BootID(), key, last, e, up, rm)
				if !fits {
					t.Fatalf("scope delta did not fit")
				}
				scopeSum += sseBytes([]sessionstream.Event{ev})
				events++
			}
			last = e
		}
		return rootsSum, scopeSum, events, last, cur
	}
	rClosed, _, _, epoch3, cur := burst("closed", next, epoch2, false)
	t.Logf("BURST 8 child mutations, drawer CLOSED: roots-deltas %d B total (%d B/mutation); full-tx would be %d B", rClosed, rClosed/8, 8*fullBytes)

	// --- same burst, drawer OPEN on DRIVER (scope registered) --------------
	regEpoch, ok := fanout.UpdateScopes(conn, []string{scopeChildrenPrefix + rows[driver].ID}, nil)
	if !ok || regEpoch != epoch3 {
		t.Fatalf("scope registration epoch %d ok=%v (want %d)", regEpoch, ok, epoch3)
	}
	rOpen, sOpen, _, epoch4, cur := burst("open", cur, epoch3, true)
	t.Logf("BURST 8 child mutations, drawer OPEN:   roots-deltas %d B + scope-deltas %d B = %d B total (%d B/mutation)", rOpen, sOpen, rOpen+sOpen, (rOpen+sOpen)/8)
	_, _ = fanout.UpdateScopes(conn, nil, []string{scopeChildrenPrefix + rows[driver].ID})

	// --- reconnect after a gap (roots view) --------------------------------
	last := epoch4
	for _, gap := range []struct {
		label     string
		mutations int
	}{{"30 s (1 mutation)", 1}, {"5 min (3 mutations)", 3}, {"30 min (16 mutations)", 16}, {"2 h (62 mutations)", 62}, {"8 h (250 mutations)", 250}} {
		resumeFrom := last
		list := cur
		for i := 0; i < gap.mutations; i++ {
			list = clone(list)
			// realistic churn: mostly children (subagent exhaust), some roots
			j := (i * 7) % len(list)
			list[j].Title = fmt.Sprintf("churn-%d", i)
			list[j].LastOutputAt = time.Now().UTC().Format(time.RFC3339)
			e, _ := broadcast(list)
			last = e
		}
		cur = list
		memo, _ := fanout.CurrentMemo()
		rb, n := deltaBytes(resumeFrom, memo, deltaClassRoots)
		fb, _ := deltaBytes(resumeFrom, memo, deltaClassAll)
		t.Logf("RECONNECT after %s: full bootstrap %d B | full-view resume %d B | roots bootstrap %d B | roots resume %d B (%d roots touched)", gap.label, fullBytes, fb, rootsBytes, rb, n)
	}

	// --- children pages -----------------------------------------------------
	memo, _ := fanout.CurrentMemo()
	annotated, family := memo.Annotated()
	pageBytes := func(id string, descendants bool, limit int) (pages int, total int, bytes int, first int) {
		cursor := ""
		for {
			page, ok := wire.ListChildren(annotated, family, id, descendants, cursor, limit)
			if !ok {
				t.Fatalf("no such session %s", id)
			}
			b, _ := json.Marshal(map[string]any{"ok": true, "data": page})
			bytes += len(b)
			if pages == 0 {
				first = len(b)
			}
			pages++
			total = page.Total
			if page.NextCursor == "" {
				return
			}
			cursor = page.NextCursor
		}
	}
	for _, tc := range []struct {
		label string
		id    string
	}{{"DRIVER", rows[driver].ID}, {"club-3090@gmux-hs", rows[club].ID}} {
		p, total, b, first := pageBytes(tc.id, false, 100)
		t.Logf("CHILDREN %s direct: %d rows, %d pages of 100, %d B total (first page %d B)", tc.label, total, p, b, first)
		p, total, b, first = pageBytes(tc.id, true, 100)
		t.Logf("CHILDREN %s descendants: %d rows, %d pages of 100, %d B total (first page %d B)", tc.label, total, p, b, first)
		p, total, b, first = pageBytes(tc.id, false, 25)
		t.Logf("CHILDREN %s direct, pages of 25: %d rows, %d pages, %d B total (first page %d B)", tc.label, total, p, b, first)
	}

	// --- project_index churn: insert one session into DRIVER's project ------
	// A new placed root in a busy project renumbers its siblings' project_index.
	{
		before, _ := fanout.CurrentMemo()
		list := clone(cur)
		newRoot := wire.Session{ID: "zz-new-root", CreatedAt: time.Now().UTC().Format(time.RFC3339), Alive: true, Adapter: "pi", SemanticAgent: true, ProjectSlug: rows[driver].ProjectSlug, ProjectIndex: 0}
		list = append(list, newRoot)
		// renumber siblings as wire.Converter would (positional)
		idx := 1
		for i := range list {
			if list[i].ProjectSlug == newRoot.ProjectSlug && list[i].ID != newRoot.ID && list[i].ParentSessionID == "" {
				list[i].ProjectIndex = idx
				idx++
			}
		}
		_, m := broadcast(list)
		rb, n := deltaBytes(before.epoch, m, deltaClassRoots)
		fb, nf := deltaBytes(before.epoch, m, deltaClassAll)
		t.Logf("PROJECT_INDEX churn (new root in %q): roots-delta %d B (%d rows) | full-delta %d B (%d rows)", newRoot.ProjectSlug, rb, n, fb, nf)
	}

	// --- CPU per broadcast at real N ---------------------------------------
	measure := func(label string, setup func(*sseFanout)) {
		f := newSSEFanout()
		f.SetOwnershipFilter(func(string) bool { return false })
		setup(f)
		l := clone(rows)
		f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l}})
		runtime.GC()
		const iters = 20
		start := time.Now()
		for i := 0; i < iters; i++ {
			l2 := clone(l)
			l2[i].Title = fmt.Sprintf("cpu-%d", i)
			f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: l2}})
		}
		t.Logf("CPU/broadcast at N=%d, %s: %v", len(rows), label, time.Since(start)/iters)
	}
	measure("deltas disabled (today's fanout)", func(f *sseFanout) { f.DisableDeltas() })
	measure("ring idle, no class demanded (nothing hashed or annotated)", func(f *sseFanout) {})
	measure("ring, roots class", func(f *sseFanout) { f.DemandClass(deltaClassRoots) })
	measure("ring, roots + 1 children scope (DRIVER)", func(f *sseFanout) {
		f.DemandClass(deltaClassRoots)
		c, _ := f.RegisterConn()
		f.UpdateScopes(c, []string{scopeChildrenPrefix + rows[driver].ID}, nil)
	})
	measure("ring, roots + club-3090 scope (686 direct children)", func(f *sseFanout) {
		f.DemandClass(deltaClassRoots)
		c, _ := f.RegisterConn()
		f.UpdateScopes(c, []string{scopeChildrenPrefix + rows[club].ID}, nil)
	})
	measure("ring, full class (spike's cost)", func(f *sseFanout) { f.DemandClass(deltaClassAll) })
	measure("ring, roots + roots-owned (hub with a 3.0 spoke attached)", func(f *sseFanout) {
		f.DemandClass(deltaClassRoots)
		f.DemandClass(deltaClassRootsOwned)
	})
	measure("ring, roots + full (a 2.x tab and a 3.0 tab side by side)", func(f *sseFanout) {
		f.DemandClass(deltaClassRoots)
		f.DemandClass(deltaClassAll)
	})
	// encode costs
	{
		m := newSessionEncodeMemo(1, &wire.SessionsPayload{Sessions: clone(rows)})
		start := time.Now()
		m.Annotated()
		annotate := time.Since(start)
		start = time.Now()
		_, _ = m.Proto3(deltaClassRoots)
		encRoots := time.Since(start)
		start = time.Now()
		_, _ = m.Proto3(deltaClassAll)
		encFull := time.Since(start)
		t.Logf("ENCODE: annotate+index %v | roots transaction %v | full transaction %v", annotate, encRoots, encFull)
	}

	// --- ring memory --------------------------------------------------------
	fanout.mu.Lock()
	entries, ids, bytes := len(fanout.ring.entries), fanout.ring.ids, fanout.ring.approxBytes()
	fanout.mu.Unlock()
	t.Logf("RING after %d epochs: %d entries, %d retained ids, ~%d B", entries, entries, ids, bytes)
}
