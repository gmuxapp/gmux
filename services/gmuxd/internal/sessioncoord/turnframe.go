package sessioncoord

import (
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// TurnFrame is the runner-held turn record (docs/runner-hook-protocol.md,
// ADR 0027's 2026-07-28 amendment), as this daemon retains it.
//
// The shape is duplicated rather than imported: cli/gmux and services/gmuxd are
// separate modules and the runner protocol is a wire contract, exactly like
// runnerIncarnationHeader and the delivery vocabulary.
//
// It is runtime-only, generation-scoped, and never written to the store: the
// frame is ADR 0011's runner-owned live truth relayed one hop further, not a row
// column. One retained copy per generation, shared by pointer rather than
// duplicated into each waiter's queue, so the budget is per-session.
type TurnFrame struct {
	Seq     uint64       `json:"seq"`
	Current *TurnCurrent `json:"current,omitempty"`
	Last    *TurnClose   `json:"last,omitempty"`
}

type TurnExchange struct {
	Ordinal     uint64 `json:"ordinal"`
	User        string `json:"user"`
	SourceBytes int    `json:"source_bytes,omitempty"`
	Iterations  int    `json:"iterations,omitempty"`
}

// TurnCurrent is the open turn's identity and inputs so far.
type TurnCurrent struct {
	TurnSeq           uint64         `json:"turn_seq"`
	PreviousExchanges *int           `json:"previous_exchanges,omitempty"`
	Exchanges         []TurnExchange `json:"exchanges,omitempty"`
	OmittedExchanges  int            `json:"omitted_exchanges,omitempty"`
	OmittedBytes      int            `json:"omitted_bytes,omitempty"`
}

// TurnClose is a settled turn's asserted result. Output is present only for a
// completed turn and is omitted rather than empty: absence means the turn
// produced no prose, never that the transport lost it.
type TurnClose struct {
	TurnSeq uint64 `json:"turn_seq"`
	Outcome string `json:"outcome"`
	// Trigger is decoded only for rolling compatibility with pre-exchange
	// runners, whose close had prose and a trigger but no Exchanges.
	Trigger           string         `json:"trigger,omitempty"`
	PreviousExchanges *int           `json:"previous_exchanges,omitempty"`
	Exchanges         []TurnExchange `json:"exchanges,omitempty"`
	OmittedExchanges  int            `json:"omitted_exchanges,omitempty"`
	OmittedBytes      int            `json:"omitted_bytes,omitempty"`
	Output            string         `json:"output,omitempty"`
	Truncated         bool           `json:"truncated,omitempty"`
	Diagnostic        string         `json:"diagnostic,omitempty"`
}

// CurrentTurnSeq reports the running turn's identity, or 0 when no turn is open
// (or no frame exists). 0 is "unknown" everywhere: it matches no close, so a
// waiter holding it is served result-free rather than another turn's answer.
func (f *TurnFrame) CurrentTurnSeq() uint64 {
	if f == nil || f.Current == nil {
		return 0
	}
	return f.Current.TurnSeq
}

// ClosedTurn returns the settled record for turnSeq, or nil when the frame's
// last close describes a different turn (two back-to-back turns between looks)
// or when turnSeq is unknown. This is the whole attribution rule: a result is
// served only on an exact identity match, and a mismatch degrades honestly to a
// result-free close.
func (f *TurnFrame) ClosedTurn(turnSeq uint64) *TurnClose {
	if f == nil || f.Last == nil || turnSeq == 0 || f.Last.TurnSeq != turnSeq {
		return nil
	}
	return f.Last
}

// setFrame retains a generation's newest frame in registry runtime. It reports
// false for an absent or replaced generation, so a frame from a runner that has
// been taken over can never be served for its successor.
//
// Frames are applied in stream order by the drain loop, and a turn edge arrives
// as ONE event carrying both the settled frame and the status that closes the
// turn (drain retains the frame before applying those facts), so a waiter that
// resolves on that close finds the settled frame already retained. The frame
// cannot arrive separately from — or later than — the close it belongs to.
func (r *Registry) setFrame(id centralstore.SessionID, generation uint64, frame *TurnFrame) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.Generation != generation {
		return false
	}
	e.Frame = frame
	r.entries[id] = e
	return true
}

// Frame returns the retained frame for the installed generation of id, or nil.
func (r *Registry) Frame(id centralstore.SessionID) *TurnFrame {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.superseded {
		return nil
	}
	return e.Frame
}

// frameOf is the publish-time lookup: it stamps the retained frame onto an
// outcome so a waiter resolving a close reads the facts the runner asserted for
// it, rather than re-reading anything after the fact.
func (c *Coordinator) frameOf(id centralstore.SessionID) *TurnFrame {
	return c.registry.Frame(id)
}
