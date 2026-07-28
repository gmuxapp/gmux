package session

import (
	"encoding/json"
	"strings"
	"unicode"

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
type TurnCurrent struct {
	TurnSeq uint64 `json:"turn_seq"`
	// Trigger is the excerpt of what started the turn.
	Trigger string `json:"trigger,omitempty"`
	// Injections are the user messages that entered the running loop after it
	// started (steers, and follow-ups pi merged into the loop). They accumulate
	// across the run: each one changes what the turn's answer means. The list is
	// BOUNDED (maxTurnInjections) and keeps the newest, so it is report material,
	// not a count.
	Injections []TurnInjection `json:"injections,omitempty"`
	// InjectionCount is how many messages have entered this loop in total, and it
	// never trims. Novelty is decided against this number rather than against
	// len(Injections): once the bounded list saturates, its length stops growing,
	// and a length-based baseline would make every later injection invisible to a
	// wait that armed at that point — which would serve it the merged answer under
	// exit 0, the one direction this rule may not fail in.
	InjectionCount uint64 `json:"injection_count,omitempty"`
}

// TurnInjection is one user message that entered a running loop.
//
// DeliveryID is the identity of the gmux request that delivered it, correlated
// at the runner (see State.NotePendingInjection). It is what lets an injecting
// caller be excluded from the interruption its own steer causes: the injector
// may claim the merged close only while its message is the loop's LAST
// injection (ADR 0027, "Steering interrupts waits"). It is empty for an
// injection gmux did not deliver — a human typing into the TUI, or raw `gmux
// send` — which is exactly the case that interrupts every waiter.
type TurnInjection struct {
	Text       string `json:"text,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty"`
}

// UnmarshalJSON accepts both the object shape and the bare excerpt string a
// runner predating delivery identity sends. The lenient direction is the one
// that matters: a frame that failed to decode wholesale would cost every result
// in it, which is the loss the frame exists to prevent.
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

// TurnClose is a settled turn's asserted result.
type TurnClose struct {
	TurnSeq uint64 `json:"turn_seq"`
	Outcome string `json:"outcome"`
	// Trigger/Injections are carried over from the turn's open record so a
	// report can name what the closed turn was asked to do.
	Trigger    string          `json:"trigger,omitempty"`
	Injections []TurnInjection `json:"injections,omitempty"`
	// InjectionCount is the closed turn's total injection count, carried over
	// from its open record for the same reason it exists there.
	InjectionCount uint64 `json:"injection_count,omitempty"`
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

// maxTurnInjections bounds the frame: a turn steered a pathological number of
// times must not grow the runner's memory without limit. The newest injections
// are the ones that matter (the last injection is the one that owns the
// answer), so the oldest are dropped.
const maxTurnInjections = 64

// maxPendingInjections bounds the correlation window (see
// NotePendingInjection). Semantic deliveries into one running turn are
// serialized by the runner's delivery slot and acknowledged within a message
// round trip, so the queue is short by construction; the cap only stops a
// pathological caller from growing it.
const maxPendingInjections = 8

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
	Frame *TurnFrame `json:"turn_frame,omitempty"`
}

// OpenTurn records an adapter-asserted turn start and marks the session active.
//
// The frame update and the status write share one critical section AND one
// event: a subscriber can never see the active edge without the turn identity
// that edge belongs to (it would then have no seq to match the close against and
// would resolve result-free), and never the identity without the edge.
func (s *State) OpenTurn(turnSeq uint64, trigger string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A delivery still armed when a turn STARTS was the thing that started it:
	// its text is this turn's trigger, not an injection into it, so the
	// correlation window closes here rather than waiting to match a message that
	// can no longer arrive.
	s.pendingInjections = nil
	frame := s.publishFrameLocked(&TurnFrame{
		Current: &TurnCurrent{TurnSeq: turnSeq, Trigger: trigger},
		Last:    s.lastClosedLocked(),
	})
	status := &adapter.Status{Active: true}
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
	s.emitTurnEdgeLocked(status, frame)
}

// NoteInjection records a user message that entered the running loop. It
// applies only to the turn it names: an injection reported against a turn that
// has already closed (or against a different one) is stale and dropped rather
// than attached to the wrong answer.
// The injection is correlated with a pending semantic delivery (by the text the
// adapter reported, see matchPendingLocked) so the request that injected it can
// tell its own message from somebody else's. excerpted is the adapter's own
// assertion that `text` is a capped prefix of the message rather than the whole
// of it — the one thing that licenses a prefix comparison.
func (s *State) NoteInjection(turnSeq uint64, text string, excerpted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.currentTurnLocked()
	if cur == nil || cur.TurnSeq != turnSeq || text == "" {
		return
	}
	next := &TurnCurrent{TurnSeq: cur.TurnSeq, Trigger: cur.Trigger, InjectionCount: cur.InjectionCount + 1}
	inj := TurnInjection{Text: text, DeliveryID: s.matchPendingLocked(text, excerpted)}
	next.Injections = append(append([]TurnInjection(nil), cur.Injections...), inj)
	if len(next.Injections) > maxTurnInjections {
		next.Injections = next.Injections[len(next.Injections)-maxTurnInjections:]
	}
	// No status transition accompanies an injection: the turn was already active
	// and stays active, so this is the one frame update that travels alone. A
	// dropped injection costs a report detail, never an attribution.
	s.emitFrameLocked(s.publishFrameLocked(&TurnFrame{Current: next, Last: s.lastClosedLocked()}))
}

// NotePendingInjection arms the correlation window for one semantic delivery:
// gmux wrote `text` to the agent's composer on behalf of the request identified
// by deliveryID, and if that text turns up as an injection on the running loop,
// the injection MAY be that request's — see matchPendingLocked for when gmux is
// willing to say so.
//
// What this is NOT: a cryptographic identity carried through the agent. pi's
// injection report contains the message text and nothing else, so the only
// evidence available is textual, and text cannot prove which source produced
// identical text. The correlation is therefore deliberately conservative and
// biased toward refusing to answer: an ambiguous report is attributed to nobody,
// and the injectors involved report an indeterminate result rather than one of
// them claiming a close its own text may never have entered.
func (s *State) NotePendingInjection(deliveryID, text string) {
	if deliveryID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingInjections = append(s.pendingInjections, TurnInjection{Text: normalizeExcerpt(text), DeliveryID: deliveryID})
	if len(s.pendingInjections) > maxPendingInjections {
		s.pendingInjections = s.pendingInjections[len(s.pendingInjections)-maxPendingInjections:]
	}
}

// DropPendingInjection withdraws a delivery whose write did not go out whole.
// Nothing of it reached the loop, so leaving it armed could only mis-attribute
// somebody else's later message to this request.
func (s *State) DropPendingInjection(deliveryID string) {
	if deliveryID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.pendingInjections[:0]
	for _, p := range s.pendingInjections {
		if p.DeliveryID != deliveryID {
			kept = append(kept, p)
		}
	}
	s.pendingInjections = kept
}

// matchPendingLocked consumes and returns the delivery id of the pending
// delivery the adapter's injection report UNAMBIGUOUSLY names, or "" when there
// is no such delivery. Caller must hold s.mu.
//
// Two rules, and both exist because the alternative fails in the one direction
// this design promises never to fail — a caller being served a close its text
// may never have entered:
//
//   - MATCHING IS EXACT, unless the adapter SAYS it truncated. Applying prefix
//     tolerance unconditionally let any SHORTER message consume a longer pending
//     delivery's identity: a human typing "stop" while "stop and say kiwi" was in
//     flight would be recorded as that caller's injection, and the caller — now
//     "acknowledged and last" — would be handed the merged answer under exit 0,
//     which is precisely the settle-before-ack case that must read as
//     indeterminate.
//
//     `excerpted` is the adapter's explicit flag on the injection event, not a
//     shape read off the text. Sniffing the trailing ellipsis instead cannot
//     distinguish a capped excerpt from a message that merely ends in one, so a
//     foreign message ending in "…" — typed, pasted, or emitted by another tool —
//     could still buy itself the prefix rule and steal an in-flight delivery's
//     identity. The adapter is the only party that knows whether it capped, so it
//     is the party that says so.
//
//   - MATCHING MUST BE UNIQUE. If two pending deliveries could explain the same
//     report (identical texts, or a truncated excerpt that prefixes both), the
//     report names nobody: no id is assigned and nothing is consumed. Both
//     injectors then resolve indeterminate and every bystander is interrupted by
//     an id-less injection. Ambiguity always degrades toward "nobody claims the
//     close", never toward an arbitrary first match, because a first match is a
//     coin flip on whose answer this is.
//
// The irreducible residue is a human typing text byte-identical to an in-flight
// steer at that same moment: pi reports one message with one text, and no
// evidence exists that could separate them. That is accepted and recorded in
// ADR 0027 rather than papered over.
func (s *State) matchPendingLocked(text string, excerpted bool) string {
	reported := normalizeExcerpt(text)
	if reported == "" {
		return ""
	}
	// Only a report the adapter FLAGGED as capped is compared as a prefix, and
	// only then is its ellipsis marker stripped: on an unflagged report the
	// ellipsis is part of the message, and trimming it would compare a string the
	// agent never saw.
	prefix := reported
	if excerpted {
		prefix = strings.TrimSuffix(reported, excerptEllipsis)
		if prefix == "" {
			return ""
		}
	}
	match := -1
	for i, p := range s.pendingInjections {
		var names bool
		if excerpted {
			names = strings.HasPrefix(p.Text, prefix)
		} else {
			names = p.Text == reported
		}
		if !names {
			continue
		}
		if match >= 0 {
			// Ambiguous. Consume nothing: leaving both records armed keeps them
			// available for a later report that IS unambiguous, and costs only
			// the (already safe) indeterminate answer in the meantime.
			return ""
		}
		match = i
	}
	if match < 0 {
		return ""
	}
	id := s.pendingInjections[match].DeliveryID
	s.pendingInjections = append(s.pendingInjections[:match:match], s.pendingInjections[match+1:]...)
	return id
}

// excerptEllipsis is the marker the adapter appends to an excerpt it capped. It
// is a DISPLAY detail: what licenses a prefix comparison is the adapter's
// explicit truncation flag, and this constant only strips the marker back off
// before comparing a flagged excerpt against the delivered text.
const excerptEllipsis = "\u2026"

// normalizeExcerpt mirrors the adapter's excerpt normalization so both sides of
// the correlation compare the same string.
//
// "Mirrors" is a contract, not a coincidence, and the two runtimes disagree by
// default: JavaScript's \s matches U+FEFF but not U+0085, while Go's
// unicode.IsSpace matches U+0085 but not U+FEFF. Both sides therefore collapse
// the UNION (see pi-ext.mjs's excerpt()), so a prompt containing either
// character still correlates instead of silently degrading its injector to an
// indeterminate result.
func normalizeExcerpt(s string) string {
	return strings.Join(strings.FieldsFunc(s, isExcerptSpace), " ")
}

// NormalizeExcerpt exposes that normalization so the adapter-side test can pin
// the two runtimes against each other rather than against a copy of the rule.
func NormalizeExcerpt(s string) string { return normalizeExcerpt(s) }

func isExcerptSpace(r rune) bool {
	return unicode.IsSpace(r) || r == '\uFEFF'
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
	s.mu.Lock()
	defer s.mu.Unlock()
	// Whatever was armed can no longer enter this loop.
	s.pendingInjections = nil
	if cur := s.currentTurnLocked(); cur != nil && cur.TurnSeq == close.TurnSeq {
		// Carry the turn's inputs into the close record so a report can say
		// what the closed turn was asked to do without a second lookup. The count
		// travels with them: a waiter that only learns of a steer at the close
		// decides novelty the same way it would have live.
		close.Trigger, close.Injections, close.InjectionCount = cur.Trigger, cur.Injections, cur.InjectionCount
	}
	frame := s.publishFrameLocked(&TurnFrame{Last: &close})
	if s.Status == nil || !s.Status.Active {
		s.emitFrameLocked(frame)
		return false
	}
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
	s.emitTurnEdgeLocked(status, frame)
	return true
}

// SetStatusAbandoningTurn is the raw whole-status write (`PUT /status`), which
// belongs to nobody's turn: it is a script or a non-hook child reporting its own
// state, with no turn identity and no asserted result.
//
// When such a write closes an open turn it ABANDONS the frame's current record
// rather than leaving it. A frame that still advertises `current: {turn_seq: N}`
// on an idle session is a lie about the present, and one that grows teeth: the
// injection path gates on `current` (so a later steer would attach to a turn
// nobody is running), and the report verbs read its trigger/injections. The
// `last` record is deliberately untouched — this writer asserted no result, and
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

// ClearTurnFrame drops both records. The frame is conversation-local, not
// merely generation-local: an authoritative rebind (pi switch/new/resume/fork)
// makes the previous conversation's answer unattributable under the new ref, so
// it is cleared atomically and ordered AHEAD of the rebind's own events. A late
// subscriber can then never read the old conversation's result as the new one's.
func (s *State) ClearTurnFrame() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingInjections = nil
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
		// to; it still travels, the way an injection or a rebind clear does.
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
