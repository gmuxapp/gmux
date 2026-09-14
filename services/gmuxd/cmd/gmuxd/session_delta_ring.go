package main

import (
	"encoding/json"
	"hash/fnv"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// SPIKE (R5/R6): the bounded per-epoch changeset ring behind epoch-chained
// deltas and `since=` resume.
//
// What it stores: for every broadcast epoch, the set of session ids whose
// *visible value changed* for each audience class (browser = the whole
// payload, peer = wire.FilterOwned). It stores ids only — never rows — so a
// subscriber's delta is always built from the CURRENT payload. That is what
// makes the mechanism history-free: there is no log to replay, no ordering
// hazard between peer rows, and a delta can never publish a stale value.
//
// Membership matters as much as content: a row that leaves an audience's
// filter (ownership flip, dismissal, retention) is recorded as touched, and
// the sender — finding it absent from the current filtered payload — emits it
// as a removal. Deltas are therefore filtered exactly like snapshots.
//
// Bounds: entries and total retained ids are both capped; eviction advances
// `base`, and any subscriber older than `base` gets today's full bootstrap.

const (
	deltaClassAll   = 0 // browser audience: the unfiltered payload
	deltaClassOwned = 1 // ?as=peer audience: wire.FilterOwned
	deltaClassCount = 2

	// A spawn burst is ~4 broadcasts/s; 1024 entries is ~4 min of burst or
	// hours of ordinary churn. maxRingIDs bounds the pathological case (a
	// reindex touching every row) — 64k ids ~ 2.6 MB of strings.
	defaultMaxRingEntries = 1024
	defaultMaxRingIDs     = 64 * 1024
)

type deltaRingEntry struct {
	epoch   uint64
	touched [deltaClassCount][]string
}

type deltaRing struct {
	maxEntries int
	maxIDs     int

	// base is the newest epoch a resuming subscriber may still chain from.
	// It advances on eviction: older subscribers must re-bootstrap.
	base    uint64
	entries []deltaRingEntry
	ids     int

	prevHash   map[string]uint64
	prevMember [deltaClassCount]map[string]struct{}
}

func newDeltaRing() *deltaRing {
	r := &deltaRing{maxEntries: defaultMaxRingEntries, maxIDs: defaultMaxRingIDs, prevHash: map[string]uint64{}}
	for c := range r.prevMember {
		r.prevMember[c] = map[string]struct{}{}
	}
	return r
}

// hashSession fingerprints a row's wire value. SPIKE shortcut: it marshals
// the row (O(row) per row per broadcast, measured in the report). A real PR
// should carry a per-row revision from the store instead, which makes the
// diff O(1) per row and removes the (astronomically unlikely, but real)
// 64-bit collision hazard.
func hashSession(s wire.Session) uint64 {
	encoded, err := json.Marshal(s)
	if err != nil {
		return 0 // encode failures are quarantined by the transaction path
	}
	h := fnv.New64a()
	_, _ = h.Write(encoded)
	return h.Sum64()
}

// Record folds one broadcast into the ring. Must be called under the fanout
// mutex, once per epoch, in epoch order, for every broadcast — the chain is
// only valid if no broadcast is skipped.
func (r *deltaRing) Record(epoch uint64, payload *wire.SessionsPayload, isLocalPeer func(string) bool) {
	rows := []wire.Session(nil)
	if payload != nil {
		rows = payload.Sessions
	}
	curHash := make(map[string]uint64, len(rows))
	var curMember [deltaClassCount]map[string]struct{}
	for c := range curMember {
		curMember[c] = make(map[string]struct{}, len(rows))
	}
	for _, s := range rows {
		curHash[s.ID] = hashSession(s)
		curMember[deltaClassAll][s.ID] = struct{}{}
		if wire.IsOwnedRow(s, isLocalPeer) {
			curMember[deltaClassOwned][s.ID] = struct{}{}
		}
	}

	entry := deltaRingEntry{epoch: epoch}
	for c := 0; c < deltaClassCount; c++ {
		var touched []string
		for id := range curMember[c] {
			if _, was := r.prevMember[c][id]; !was {
				touched = append(touched, id)
				continue
			}
			if r.prevHash[id] != curHash[id] {
				touched = append(touched, id)
			}
		}
		for id := range r.prevMember[c] {
			if _, still := curMember[c][id]; !still {
				touched = append(touched, id) // removal, incl. filtered-out
			}
		}
		entry.touched[c] = touched
		r.ids += len(touched)
	}
	r.prevHash = curHash
	r.prevMember = curMember
	r.entries = append(r.entries, entry)
	r.evict()
}

func (r *deltaRing) evict() {
	for len(r.entries) > 0 && (len(r.entries) > r.maxEntries || r.ids > r.maxIDs) {
		oldest := r.entries[0]
		for c := range oldest.touched {
			r.ids -= len(oldest.touched[c])
		}
		r.base = oldest.epoch
		r.entries = r.entries[1:]
	}
}

// TouchedSince returns the ids a subscriber last served at `from` must be
// updated on to reach `to`, for one audience class. ok is false when the
// chain no longer covers `from` (evicted, or a different daemon boot) —
// the caller must then send a full transaction.
func (r *deltaRing) TouchedSince(from, to uint64, class int) ([]string, bool) {
	if class < 0 || class >= deltaClassCount {
		return nil, false
	}
	if from == 0 || from < r.base || to < from {
		return nil, false
	}
	seen := map[string]struct{}{}
	out := []string(nil)
	for _, e := range r.entries {
		if e.epoch <= from {
			continue
		}
		if e.epoch > to {
			break
		}
		for _, id := range e.touched[class] {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, true
}

// approxBytes is the ring's retained-string footprint, for the memory
// measurement in the report.
func (r *deltaRing) approxBytes() int {
	total := 0
	for _, e := range r.entries {
		for c := range e.touched {
			for _, id := range e.touched[c] {
				total += len(id) + 16 // string header + bytes
			}
		}
	}
	for id := range r.prevHash {
		total += len(id) + 16 + 8
	}
	for c := range r.prevMember {
		for id := range r.prevMember[c] {
			total += len(id) + 16
		}
	}
	return total
}
