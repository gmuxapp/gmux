package session

import (
	"github.com/gmuxapp/gmux/packages/adapter"
)

// The turn frame is the runner's live record of what the adapter asserted about
// its turns (ADR 0027, 2026-07-28 amendment: "the result is asserted at the
// source"). gmux does not reconstruct a turn's answer from the conversation
// file; the adapter reports it, the runner holds it, and every /events
// subscriber is replayed the current frame on connect — exactly the way status,
// conversation ref and slug are already relayed (ADR 0011).
//
// Why a held frame rather than a payload riding the event edge: the daemon's
// pipe drops payloads in several legitimate places (subscriber watermarks
// coalesce overtaken publishes, the outcome publish re-reads the row
// post-commit, the generic wait has a row-snapshot ticker path that carries no
// event at all). Any of those turns into "completed, exit 0, no answer" — the
// exact failure this design exists to kill. A replayed, sequence-bearing
// snapshot converges instead.
//
// It is NOT row state, NOT persisted (it dies with the runner) and NOT a tape
// read.
type TurnFrame struct {
	// Seq is a frame version, monotonic per runner generation. It lets a
	// consumer tell a replayed frame from a stale one without comparing
	// contents.
	Seq uint64 `json:"seq"`
	// Current is the turn that is running right now, or nil when none is.
	Current *TurnCurrent `json:"current,omitempty"`
	// Last is the most recent CLOSED turn, or nil when none has closed in this
	// conversation. Kept apart from Current so a reader can never pair a
	// running turn's trigger with the previous turn's answer.
	Last *TurnClose `json:"last,omitempty"`
}

// TurnCurrent is the open turn's identity and inputs so far.
type TurnExchange struct {
	Ordinal     uint64 `json:"ordinal"`
	User        string `json:"user"`
	SourceBytes int    `json:"source_bytes,omitempty"`
	Iterations  int    `json:"iterations,omitempty"`
}

type TurnCurrent struct {
	TurnSeq uint64 `json:"turn_seq"`
	// PreviousExchanges is the active branch's exchange count before this
	// activity. It permits boundary-based persistence reconciliation.
	PreviousExchanges *int `json:"previous_exchanges,omitempty"`
	// Exchanges is the source-observed, user-bounded activity span.
	Exchanges        []TurnExchange `json:"exchanges,omitempty"`
	OmittedExchanges int            `json:"omitted_exchanges,omitempty"`
	OmittedBytes     int            `json:"omitted_bytes,omitempty"`
}

// TurnClose is a settled turn's asserted result.
type TurnClose struct {
	TurnSeq           uint64         `json:"turn_seq"`
	Outcome           string         `json:"outcome"`
	PreviousExchanges *int           `json:"previous_exchanges,omitempty"`
	Exchanges         []TurnExchange `json:"exchanges,omitempty"`
	OmittedExchanges  int            `json:"omitted_exchanges,omitempty"`
	OmittedBytes      int            `json:"omitted_bytes,omitempty"`
	// Output is the settled turn's final assistant prose, present only for a
	// completed turn and OMITTED (never empty) when the turn produced none: an
	// absent output means a tool-only turn, never transport loss.
	Output string `json:"output,omitempty"`
	// Truncated records that the adapter capped Output at the source.
	Truncated bool `json:"truncated,omitempty"`
	// Diagnostic is a short reason for a non-completed close — the account
	// channel, never the result.
	Diagnostic string `json:"diagnostic,omitempty"`
}

// maxLiveInstructions bounds the retained activity span. Together with the
// opening boundary this keeps the newest 65 exchanges.
const maxLiveInstructions = 64

// turnEdge is the wire payload of a turn edge: a status transition and the turn
// frame that transition belongs to, in ONE event.
//
// One event, not two, is the whole point. The runner's fan-out is lossy by
// design (State.emit drops into a full subscriber buffer rather than stalling
// the runner), so a frame emitted separately from its status edge can be dropped
// while the edge is delivered — a subscriber then sees a close it cannot
// attribute, which is precisely the "completed, exit 0, no answer" phenotype
// this design exists to kill. Coupling them into a single send makes that
// unobservable: a subscriber gets the edge WITH its frame, or gets neither and
// converges on the next event or on reconnect replay.
//
// The embedded *adapter.Status inlines its own JSON fields, so the event stays
// wire-compatible with a consumer that only knows about status.
type turnEdge struct {
	*adapter.Status
	Frame       *TurnFrame `json:"turn_frame,omitempty"`
	Unread      *bool      `json:"unread,omitempty"`
	UnreadToken *string    `json:"unread_token,omitempty"`
}

// OpenTurn records an adapter-asserted turn start and marks the session active.
//
// The frame update and the status write share one critical section AND one
// event: a subscriber can never see the active edge without the turn identity
// that edge belongs to (it would then have no seq to match the close against and
// would resolve result-free), and never the identity without the edge.
func (s *State) OpenTurn(turnSeq uint64, trigger string, sourceBytes int) {
	s.OpenTurnSource(turnSeq, trigger, sourceBytes, nil)
}

// OpenTurnSource retains source facts asserted before user-boundary capping.
func (s *State) OpenTurnSource(turnSeq uint64, trigger string, sourceBytes int, previousExchanges *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := s.publishFrameLocked(&TurnFrame{
		Current: &TurnCurrent{TurnSeq: turnSeq, PreviousExchanges: previousExchanges, Exchanges: []TurnExchange{{Ordinal: 1, User: trigger, SourceBytes: sourceBytes}}},
		Last:    s.lastClosedLocked(),
	})
	status := &adapter.Status{Active: true}
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
	s.emitTurnEdgeLocked(status, frame)
}

// NoteInjection records a user message that entered the running loop. It
// applies only to the turn it names: a message for a closed or different turn
// is stale and is dropped. sourceBytes is the uncapped UTF-8 byte length
// asserted by the adapter; it keeps omission accounting exact.
func (s *State) NoteInjection(turnSeq uint64, text string, sourceBytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.currentTurnLocked()
	if cur == nil || cur.TurnSeq != turnSeq || text == "" {
		return
	}
	next := &TurnCurrent{TurnSeq: cur.TurnSeq, PreviousExchanges: cur.PreviousExchanges,
		Exchanges: append([]TurnExchange(nil), cur.Exchanges...), OmittedExchanges: cur.OmittedExchanges, OmittedBytes: cur.OmittedBytes}
	ordinal := uint64(1)
	if n := len(next.Exchanges); n > 0 {
		ordinal = next.Exchanges[n-1].Ordinal + 1
	}
	next.Exchanges = append(next.Exchanges, TurnExchange{Ordinal: ordinal, User: text, SourceBytes: sourceBytes})
	// No status transition accompanies an additional user boundary: the activity
	// stays open, so this frame update travels alone.
	if len(next.Exchanges) > maxLiveInstructions+1 {
		dropped := next.Exchanges[0]
		next.Exchanges = next.Exchanges[1:]
		next.OmittedExchanges++
		if dropped.SourceBytes > 0 {
			next.OmittedBytes += dropped.SourceBytes
		} else {
			next.OmittedBytes += len([]byte(dropped.User))
		}
	}
	s.emitFrameLocked(s.publishFrameLocked(&TurnFrame{Current: next, Last: s.lastClosedLocked()}))
}

// NoteIteration records one completed assistant/model response in the latest
// visible exchange. It is a frame-only update: activity remains open.
func (s *State) NoteIteration(turnSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.currentTurnLocked()
	if cur == nil || cur.TurnSeq != turnSeq || len(cur.Exchanges) == 0 {
		return
	}
	next := *cur
	next.Exchanges = append([]TurnExchange(nil), cur.Exchanges...)
	next.Exchanges[len(next.Exchanges)-1].Iterations++
	s.emitFrameLocked(s.publishFrameLocked(&TurnFrame{Current: &next, Last: s.lastClosedLocked()}))
}

// CloseTurnFrame atomically records a settled turn's asserted result and closes
// the open turn with the terminal status. It reports whether it closed a turn.
// It is the ONLY close path in the runner: every terminal turn state goes
// through here, so no writer can close a turn without carrying (or deliberately
// omitting) the result that closed it.
//
// A terminal end is only meaningful while a turn is OPEN. An end delivered
// against an already-closed turn is stale — a duplicate, or a hook that fires
// unconditionally on exit, like Claude's SessionEnd after Stop — and must not
// rewrite a good closure, because Interrupted and Error are durable facts.
//
// The check and the write share one critical section on purpose. A caller cannot
// do StatusSnapshot-then-write: two concurrent ends (hook POSTs are independent
// HTTP requests on their own goroutines) could both observe the open turn and
// both write, and a turn start could interleave between the check and the write.
//
// The status half is POLARITY, not turn identity: it cannot recognize a
// *logically* stale end that arrives after a NEW turn already started, and would
// close the new turn with it. Excluding that ordering is the sender's job — see
// the delivery serialization in pi-ext.mjs and Claude's sequential hook
// execution. Turn IDENTITY is carried separately, by the frame's turn_seq, which
// is what decides whether a waiter may use the result.
//
// Atomicity is the point, and it is stronger than ordering: the settled frame
// and the terminal status travel as ONE event (see turnEdge), so no subscriber
// can observe the close without the frame that closed it — not by reordering,
// and not by the fan-out dropping one of two sends into a full buffer. That is
// what makes the scoped delivery invariant ("a live result-bearing wait never
// resolves completed without the settled frame") a property of the transport
// rather than a hope about buffer occupancy.
//
// The close record is written even when no turn was open to close (a duplicate
// or late end): it is the adapter's assertion about a turn identity, and the
// turn_seq match downstream decides whether anyone may use it. Such a stale end
// still publishes the frame — alone, since there is no status transition to pair
// it with.
func (s *State) CloseTurnFrame(close TurnClose, status *adapter.Status) bool {
	return s.CloseTurnFrameUnread(close, status, false)
}

// CloseTurnFrameUnread publishes unread and the terminal status as one edge
// when this close produced a consumable result. A waiter resolving on that edge
// receives the exact token it must acknowledge, while notification suppression
// is decided before the unread fact can schedule delivery.
func (s *State) CloseTurnFrameUnread(close TurnClose, status *adapter.Status, unread bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Whatever was armed can no longer enter this loop.
	if cur := s.currentTurnLocked(); cur != nil && cur.TurnSeq == close.TurnSeq {
		// Carry the turn's inputs into the close record so a report can say
		// what the closed turn was asked to do without a second lookup. The count
		// travels with them: a waiter that only learns of a steer at the close
		// decides novelty the same way it would have live.
		close.PreviousExchanges = cur.PreviousExchanges
		close.Exchanges, close.OmittedExchanges, close.OmittedBytes = cur.Exchanges, cur.OmittedExchanges, cur.OmittedBytes
	}
	frame := s.publishFrameLocked(&TurnFrame{Last: &close})
	if s.Status == nil || !s.Status.Active {
		s.emitFrameLocked(frame)
		return false
	}
	if unread {
		s.markUnreadResultStateLocked()
	}
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
	if unread {
		value := true
		token := s.UnreadToken
		s.emit(Event{Type: "status", Data: turnEdge{Status: status, Frame: frame, Unread: &value, UnreadToken: &token}})
	} else {
		s.emitTurnEdgeLocked(status, frame)
	}
	return true
}

// SetStatusAbandoningTurn is the raw whole-status write (`PUT /status`), which
// belongs to nobody's turn: it is a script or a non-hook child reporting its own
// state, with no turn identity and no asserted result.
//
// When such a write closes an open turn it ABANDONS the frame's current record
// rather than leaving it. A frame that still advertises `current: {turn_seq: N}`
// on an idle session is a lie about the present. The `last` record is
// deliberately untouched — this writer asserted no result, and
// inventing a close record for it would put a turn_seq into `last` that some
// waiter could match against an answer that does not exist.
//
// So the effect on attribution is exactly what it was before: a waiter's
// ClosedTurn(N) finds the PREVIOUS close, mismatches, and resolves result-free.
// This only stops the frame from describing a turn that has ended.
//
// Like every other turn edge, the frame update and the status write are one
// event, so a subscriber cannot see the close without the frame it produced.
func (s *State) SetStatusAbandoningTurn(status *adapter.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
	active := status != nil && status.Active
	if active || s.currentTurnLocked() == nil {
		// Nothing to abandon: either the write keeps the session active, or no
		// turn was ever asserted (a shell session, or one whose turn already
		// closed). Stays a plain status event.
		s.emit(Event{Type: "status", Data: status})
		return
	}
	s.emitTurnEdgeLocked(status, s.publishFrameLocked(&TurnFrame{Last: s.lastClosedLocked()}))
}

// ClearConversationState drops every conversation-local runtime fact before an
// authoritative rebind is published. Outcome status and the frame must move
// together: retaining either can classify conversation B with conversation A's
// failed/interrupted close.
func (s *State) ClearConversationState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = nil
	if s.turnFrame == nil || (s.turnFrame.Current == nil && s.turnFrame.Last == nil) {
		return
	}
	s.emitFrameLocked(s.publishFrameLocked(&TurnFrame{}))
}

// TurnFrameSnapshot returns the held frame for replay to a (re)connecting
// /events subscriber, or nil when nothing has been asserted yet. The frame is
// immutable once published — writers replace the pointer — so it is shared, not
// copied: one bounded frame per runner, whatever the number of subscribers.
func (s *State) TurnFrameSnapshot() *TurnFrame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.turnFrame
}

// TurnEdgeSnapshot returns the status and the turn frame as they were at ONE
// instant, for replay to a (re)connecting subscriber.
//
// Taking them together is the point. Read separately they can straddle a turn
// edge — a status from before the close with the frame from after it, or the
// reverse — and a replay is precisely where that matters: a daemon that
// reconnected mid-turn learns its turn identity here, and a torn pair leaves a
// wait armed in that window binding turn_seq 0, i.e. resolving result-free. The
// live path cannot tear because an edge is one event (see turnEdge); the replay
// gets the same guarantee from one lock.
//
// Either half may be nil: a session with no status reported yet, or one whose
// adapter asserts no turns.
func (s *State) TurnEdgeSnapshot() (*adapter.Status, *TurnFrame) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Status == nil {
		return nil, s.turnFrame
	}
	cp := *s.Status
	return &cp, s.turnFrame
}

// ReplayTurnEdge renders the replay payload for a status snapshot and the frame
// that belongs to it, in the SAME coupled shape a live edge uses: one status
// event carrying `turn_frame`. A subscriber therefore parses one shape, live or
// replayed, and cannot receive the status of a turn without the frame that
// identifies it.
//
// The bool reports whether there is anything to send at all.
func ReplayTurnEdge(status *adapter.Status, frame *TurnFrame) (typ string, payload any, ok bool) {
	switch {
	case status != nil:
		return "status", turnEdge{Status: status, Frame: frame}, true
	case frame != nil:
		// No status was ever reported, so there is no edge to couple the frame
		// to; it still travels, the way a boundary update or rebind clear does.
		return "turn_frame", frame, true
	}
	return "", nil, false
}

// publishFrameLocked stamps and installs a new frame version, and returns it for
// the caller to emit — alone (emitFrameLocked) or coupled to the status edge it
// belongs to (emitTurnEdgeLocked). Installing and emitting are separate steps
// only so the close can put both facts in one event; the frame value is never
// mutated after publication. Caller must hold s.mu.
func (s *State) publishFrameLocked(f *TurnFrame) *TurnFrame {
	s.frameSeq++
	f.Seq = s.frameSeq
	s.turnFrame = f
	return f
}

// emitFrameLocked relays a frame that has no status transition to pair with.
func (s *State) emitFrameLocked(f *TurnFrame) {
	s.emit(Event{Type: "turn_frame", Data: f})
}

// emitTurnEdgeLocked relays a status transition together with the frame it
// belongs to, as one event on the "status" channel. The type is deliberately the
// existing one: a consumer that knows nothing about frames still sees exactly
// the status event it always saw.
func (s *State) emitTurnEdgeLocked(status *adapter.Status, f *TurnFrame) {
	s.emit(Event{Type: "status", Data: turnEdge{Status: status, Frame: f}})
}

func (s *State) currentTurnLocked() *TurnCurrent {
	if s.turnFrame == nil {
		return nil
	}
	return s.turnFrame.Current
}

func (s *State) lastClosedLocked() *TurnClose {
	if s.turnFrame == nil {
		return nil
	}
	return s.turnFrame.Last
}
