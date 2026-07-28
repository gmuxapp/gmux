package sessioncoord

import (
	"encoding/json"
	"strings"

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

// TurnCurrent is the open turn's identity and inputs so far.
type TurnCurrent struct {
	TurnSeq    uint64          `json:"turn_seq"`
	Trigger    string          `json:"trigger,omitempty"`
	Injections []TurnInjection `json:"injections,omitempty"`
	// InjectionCount is the turn's TOTAL number of injections, which never
	// trims, unlike the bounded Injections list. Novelty is decided against it
	// (see InjectionsSeen).
	InjectionCount uint64 `json:"injection_count,omitempty"`
}

// InjectionsSeen reports how many messages have entered this loop, from the
// runner's monotonic counter when it sent one and from the retained list
// otherwise.
//
// The fallback is for a runner that predates the counter: its list is bounded
// too, so a saturated turn can still hide later injections there, but reading
// the list is strictly better than reading zero — which would make every
// injection invisible on that runner and hand waiters the merged answer.
func (c *TurnCurrent) InjectionsSeen() uint64 {
	if c == nil {
		return 0
	}
	if c.InjectionCount > 0 {
		return c.InjectionCount
	}
	return uint64(len(c.Injections))
}

// TurnInjection is one user message that entered the running loop: a steer, a
// follow-up the agent merged into the turn, or a human typing into the TUI.
//
// DeliveryID is the identity of the gmux request that delivered it, correlated
// at the runner. It is empty for an injection gmux did not deliver, and that
// emptiness is load-bearing: a human grabbing the wheel interrupts every
// waiter, including one that had just steered.
type TurnInjection struct {
	Text       string `json:"text,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty"`
}

// UnmarshalJSON accepts the bare excerpt string a runner predating delivery
// identity sends, as well as the object shape. Refusing it would fail the whole
// frame's decode and cost every result in it — the loss the frame exists to
// prevent — so the old shape degrades to "an injection with no identity",
// which is exactly what it is.
func (t *TurnInjection) UnmarshalJSON(b []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(b)), `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = TurnInjection{Text: s}
		return nil
	}
	type raw TurnInjection
	var v raw
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*t = TurnInjection(v)
	return nil
}

// TurnClose is a settled turn's asserted result. Output is present only for a
// completed turn and is omitted rather than empty: absence means the turn
// produced no prose, never that the transport lost it.
type TurnClose struct {
	TurnSeq        uint64          `json:"turn_seq"`
	Outcome        string          `json:"outcome"`
	Trigger        string          `json:"trigger,omitempty"`
	Injections     []TurnInjection `json:"injections,omitempty"`
	InjectionCount uint64          `json:"injection_count,omitempty"`
	Output         string          `json:"output,omitempty"`
	Truncated      bool            `json:"truncated,omitempty"`
	Diagnostic     string          `json:"diagnostic,omitempty"`
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

// TriggerExcerpt is the nil-safe read of an open turn's trigger, so a report can
// name what the turn was asked to do without every caller re-checking for a
// frame that does not exist.
func (c *TurnCurrent) TriggerExcerpt() string {
	if c == nil {
		return ""
	}
	return c.Trigger
}

// TriggerExcerpt is the nil-safe read of a settled turn's trigger.
func (t *TurnClose) TriggerExcerpt() string {
	if t == nil {
		return ""
	}
	return t.Trigger
}

// InjectionsSeen is the settled turn's total injection count, with the same
// list fallback (and the same reason) as the open record's.
func (t *TurnClose) InjectionsSeen() uint64 {
	if t == nil {
		return 0
	}
	if t.InjectionCount > 0 {
		return t.InjectionCount
	}
	return uint64(len(t.Injections))
}

// CurrentTurn returns the open turn's record for turnSeq, or nil when no turn
// with that identity is open. It is the injection-watch lookup: a waiter asks
// about the turn IT is bound to, never about "whatever is running".
func (f *TurnFrame) CurrentTurn(turnSeq uint64) *TurnCurrent {
	if f == nil || f.Current == nil || turnSeq == 0 || f.Current.TurnSeq != turnSeq {
		return nil
	}
	return f.Current
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
