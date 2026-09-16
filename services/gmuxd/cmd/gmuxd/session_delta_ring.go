package main

import (
	"encoding/json"
	"hash/fnv"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// PROTO (3.0, R5/R6 ring generalized): the bounded per-epoch changeset ring
// behind epoch-chained deltas, `since=` resume and scoped (children:<id>)
// subscriptions.
//
// What it stores: for every broadcast epoch, the set of session ids whose
// *visible value changed* for each VIEW a subscriber can hold. It stores ids
// only — never rows — so a subscriber's delta is always built from the
// CURRENT payload. That is what makes the mechanism history-free: there is no
// log to replay, no ordering hazard between peer rows, and a delta can never
// publish a stale value.
//
// Views come in two kinds:
//
//   - four static CLASSES = audience (browser | ?as=peer) × shape (full |
//     roots). A class is tracked from the first moment a subscriber demands
//     it and stays tracked (so a reconnecting subscriber can resume); an
//     undemanded class costs nothing per broadcast. The browser's roots
//     class is ~5 % of the rows of the full class, which is where the
//     spike's +12 ms/broadcast went.
//   - dynamic SCOPES (`children:<id>|<cursor>`, `session:<id>`), refcounted
//     by the connections that registered them and dropped at zero. A scope
//     is hashed only while someone is looking at it, so an idle daemon pays
//     for the sidebar only. A children scope is a VIEWPORT (round-2 D2): the
//     rows from the listing head to the cursor the client last echoed,
//     capped at scopeWindowMax — bounded by what is on screen the way the
//     world state is bounded by roots.
//
// Membership matters as much as content: a row that leaves a view (ownership
// flip, dismissal, promotion out of a subtree) is recorded as touched, and the
// sender — finding it absent from the current view — emits it as a removal.
//
// Bounds: entries and retained ids are capped, with SEPARATE id budgets for
// the view classes and for scopes (round-2 M5): the world chain's horizon
// (`base`) is only ever shortened by world churn. Scope churn trims scope
// entries from the oldest epoch forward (`scopeBase`); a scope subscriber
// older than that gets a reset for that scope, never a full bootstrap.

const (
	deltaClassAll        = 0 // browser, full payload (2.x shape)
	deltaClassOwned      = 1 // ?as=peer, full payload (wire.FilterOwned)
	deltaClassRoots      = 2 // browser, roots only (3.0 world state)
	deltaClassRootsOwned = 3 // ?as=peer, roots only (3.0 hub↔spoke link)
	deltaClassCount      = 4

	defaultMaxRingEntries = 1024
	defaultMaxRingIDs     = 64 * 1024
	defaultMaxScopeIDs    = 32 * 1024

	scopeChildrenPrefix = "children:"
	scopeSessionPrefix  = "session:"
	// scopeWindowMax caps a children scope's viewport. Pages a client holds
	// beyond it are pull-only (the registration reply says how many rows are
	// live). Chosen so a burst over a full window still has a chance to fit
	// one event next to the world delta; the fold marks the scope reset when
	// it does not.
	scopeWindowMax = 200
)

// scopeRingKey names one distinct viewport: the client-facing scope plus the
// cursor bound. Two tabs holding different depths of one family are two ring
// scopes; two tabs at the same depth share one.
func scopeRingKey(scope, cursor string) string {
	if cursor == "" {
		return scope
	}
	return scope + "|" + cursor
}

// splitScopeRingKey undoes scopeRingKey.
func splitScopeRingKey(key string) (scope, cursor string) {
	scope, cursor, _ = strings.Cut(key, "|")
	return scope, cursor
}

func deltaClassFor(peer, roots bool) int {
	switch {
	case peer && roots:
		return deltaClassRootsOwned
	case roots:
		return deltaClassRoots
	case peer:
		return deltaClassOwned
	default:
		return deltaClassAll
	}
}

// validScopeKey accepts the two scope kinds a client may register.
func validScopeKey(key string) bool {
	rest := ""
	switch {
	case strings.HasPrefix(key, scopeChildrenPrefix):
		rest = key[len(scopeChildrenPrefix):]
	case strings.HasPrefix(key, scopeSessionPrefix):
		rest = key[len(scopeSessionPrefix):]
	default:
		return false
	}
	return rest != "" && len(rest) <= 256
}

type viewState struct {
	prevHash   map[string]uint64
	prevMember map[string]struct{}
}

type scopeState struct {
	viewState
	refs int
}

type deltaRingEntry struct {
	epoch   uint64
	touched [deltaClassCount][]string
	scoped  map[string][]string
}

type deltaRing struct {
	maxEntries  int
	maxIDs      int
	maxScopeIDs int

	base      uint64
	scopeBase uint64
	entries   []deltaRingEntry
	ids       int
	scopeIDs  int

	classes [deltaClassCount]*viewState
	scopes  map[string]*scopeState
}

func newDeltaRing() *deltaRing {
	return &deltaRing{maxEntries: defaultMaxRingEntries, maxIDs: defaultMaxRingIDs, maxScopeIDs: defaultMaxScopeIDs, scopes: map[string]*scopeState{}}
}

// ScopeCount is the number of distinct viewports being tracked (metric).
func (r *deltaRing) ScopeCount() int { return len(r.scopes) }

// hashSession fingerprints a row's wire value (including its descendant
// counts, so a root whose subtree changed is touched). PROTO shortcut kept
// from the spike: it marshals the row. With the roots class this runs over
// ~150 rows per broadcast instead of ~2,900; a store-side revision would make
// it O(1)/row and is still the right end state.
func hashSession(s wire.Session) uint64 {
	encoded, err := json.Marshal(s)
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(encoded)
	return h.Sum64()
}

// rowHasher memoizes hashSession for one broadcast: a row that sits in
// several demanded views/scopes (roots + roots-owned on a hub, a 2.x and a
// 3.0 tab side by side, a `session:` scope over a root) is marshalled once
// per epoch instead of once per view. The Proto3 encode still marshals the
// row again for the wire; folding that in needs sessionstream.Encode to take
// pre-encoded rows (see the report).
type rowHasher interface {
	RowHash(s wire.Session) uint64
}

type plainHasher struct{}

func (plainHasher) RowHash(s wire.Session) uint64 { return hashSession(s) }

func snapshotView(rows []wire.Session, h rowHasher) viewState {
	if h == nil {
		h = plainHasher{}
	}
	v := viewState{prevHash: make(map[string]uint64, len(rows)), prevMember: make(map[string]struct{}, len(rows))}
	for _, s := range rows {
		v.prevHash[s.ID] = h.RowHash(s)
		v.prevMember[s.ID] = struct{}{}
	}
	return v
}

// diffView returns the ids whose visible value changed between prev and the
// rows now in the view, including ids that left it. It also returns the new
// state for the next diff.
func diffView(prev viewState, rows []wire.Session, h rowHasher) ([]string, viewState) {
	cur := snapshotView(rows, h)
	var touched []string
	for id, h := range cur.prevHash {
		if ph, was := prev.prevHash[id]; !was || ph != h {
			touched = append(touched, id)
		}
	}
	for id := range prev.prevMember {
		if _, still := cur.prevMember[id]; !still {
			touched = append(touched, id)
		}
	}
	return touched, cur
}

// DemandClass starts tracking a class (idempotent). Must be called under the
// fanout mutex with the memo of the CURRENT state, so the first diff has a
// correct baseline.
func (r *deltaRing) DemandClass(class int, memo *sessionEncodeMemo) {
	if class < 0 || class >= deltaClassCount || r.classes[class] != nil {
		return
	}
	v := viewState{prevHash: map[string]uint64{}, prevMember: map[string]struct{}{}}
	if memo != nil {
		v = snapshotView(memo.ViewRows(class), memo)
	}
	r.classes[class] = &v
}

// AddScope registers one more viewer of a scope, initializing its baseline
// from the current memo when it is new.
func (r *deltaRing) AddScope(key string, memo *sessionEncodeMemo) {
	if st, ok := r.scopes[key]; ok {
		st.refs++
		return
	}
	st := &scopeState{refs: 1, viewState: viewState{prevHash: map[string]uint64{}, prevMember: map[string]struct{}{}}}
	if memo != nil {
		st.viewState = snapshotView(memo.ScopeRows(key), memo)
	}
	r.scopes[key] = st
}

// ReleaseScope drops one viewer; at zero the scope stops being tracked.
func (r *deltaRing) ReleaseScope(key string) {
	st, ok := r.scopes[key]
	if !ok {
		return
	}
	st.refs--
	if st.refs <= 0 {
		delete(r.scopes, key)
	}
}

// Record folds one broadcast into the ring. Must be called under the fanout
// mutex, once per epoch, in epoch order, for every broadcast.
func (r *deltaRing) Record(epoch uint64, memo *sessionEncodeMemo) {
	entry := deltaRingEntry{epoch: epoch}
	for c := 0; c < deltaClassCount; c++ {
		st := r.classes[c]
		if st == nil {
			continue
		}
		var touched []string
		touched, *st = diffView(*st, memo.ViewRows(c), memo)
		entry.touched[c] = touched
		r.ids += len(touched)
	}
	if len(r.scopes) > 0 {
		entry.scoped = make(map[string][]string, len(r.scopes))
		for key, st := range r.scopes {
			var touched []string
			touched, st.viewState = diffView(st.viewState, memo.ScopeRows(key), memo)
			entry.scoped[key] = touched
			r.scopeIDs += len(touched)
		}
	}
	r.entries = append(r.entries, entry)
	r.evict()
}

func (r *deltaRing) evict() {
	for len(r.entries) > 0 && (len(r.entries) > r.maxEntries || r.ids > r.maxIDs) {
		oldest := r.entries[0]
		for c := range oldest.touched {
			r.ids -= len(oldest.touched[c])
		}
		for _, ids := range oldest.scoped {
			r.scopeIDs -= len(ids)
		}
		r.base = oldest.epoch
		if oldest.epoch > r.scopeBase {
			r.scopeBase = oldest.epoch
		}
		r.entries = r.entries[1:]
	}
	// Scope churn has its own budget: trim scope changesets from the oldest
	// entries forward, leaving the world chain intact.
	for i := 0; i < len(r.entries) && r.scopeIDs > r.maxScopeIDs; i++ {
		e := &r.entries[i]
		if e.scoped == nil {
			continue
		}
		for _, ids := range e.scoped {
			r.scopeIDs -= len(ids)
		}
		e.scoped = nil
		r.scopeBase = e.epoch
	}
}

func (r *deltaRing) touchedSince(from, to uint64, pick func(deltaRingEntry) ([]string, bool)) ([]string, bool) {
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
		ids, ok := pick(e)
		if !ok {
			return nil, false
		}
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, true
}

// TouchedSince returns the ids a subscriber of `class` last served at `from`
// must be updated on to reach `to`. ok is false when the chain no longer
// covers `from` (evicted, never tracked, or a different daemon boot).
func (r *deltaRing) TouchedSince(from, to uint64, class int) ([]string, bool) {
	if class < 0 || class >= deltaClassCount || r.classes[class] == nil {
		return nil, false
	}
	return r.touchedSince(from, to, func(e deltaRingEntry) ([]string, bool) { return e.touched[class], true })
}

// ScopeTouchedSince is TouchedSince for a dynamic scope. An epoch in the
// range that predates the scope's registration breaks the chain (ok=false):
// the client must refetch.
func (r *deltaRing) ScopeTouchedSince(from, to uint64, key string) ([]string, bool) {
	if _, tracked := r.scopes[key]; !tracked {
		return nil, false
	}
	if from < r.scopeBase {
		return nil, false
	}
	return r.touchedSince(from, to, func(e deltaRingEntry) ([]string, bool) {
		ids, ok := e.scoped[key]
		return ids, ok
	})
}

// approxBytes is the ring's retained-string footprint, for measurements.
func (r *deltaRing) approxBytes() int {
	total := 0
	for _, e := range r.entries {
		for c := range e.touched {
			for _, id := range e.touched[c] {
				total += len(id) + 16
			}
		}
		for _, ids := range e.scoped {
			for _, id := range ids {
				total += len(id) + 16
			}
		}
	}
	for _, st := range r.classes {
		if st == nil {
			continue
		}
		for id := range st.prevHash {
			total += 2*(len(id)+16) + 8
		}
	}
	for _, st := range r.scopes {
		for id := range st.prevHash {
			total += 2*(len(id)+16) + 8
		}
	}
	return total
}
