// Package session holds the in-memory session state for a single gmux-run
// instance. This is the source of truth — served via /meta and /events.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// State is the in-memory session state served by GET /meta.
type State struct {
	mu sync.RWMutex

	// Core identity (immutable after creation)
	ID            string            `json:"id"`
	CreatedAt     string            `json:"created_at"`
	Command       []string          `json:"command"`
	Cwd           string            `json:"cwd"`
	Adapter       string            `json:"adapter"`
	WorkspaceRoot string            `json:"workspace_root,omitempty"`
	Remotes       map[string]string `json:"remotes,omitempty"`

	// ParentSessionID is the session this one was spawned from (e.g.
	// `gmux edit` invoked as $EDITOR inside an existing session).
	// Empty for top-level sessions. Immutable after creation.
	ParentSessionID string `json:"parent_session_id,omitempty"`

	// Process state (owned by runner)
	Alive     bool   `json:"alive"`
	Pid       int    `json:"pid"`
	ExitCode  *int   `json:"exit_code"`
	StartedAt string `json:"started_at"`
	ExitedAt  string `json:"exited_at,omitempty"`

	// Title sources. Display title is resolved: adapter > shell > command basename.
	ShellTitle   string `json:"shell_title,omitempty"`
	AdapterTitle string `json:"adapter_title,omitempty"`

	// Other display fields
	Subtitle    string          `json:"subtitle,omitempty"`
	Status      *adapter.Status `json:"status"`
	Unread      bool            `json:"unread"`
	UnreadToken string          `json:"unread_token,omitempty"`

	// Slug is an adapter-provided stable identifier for URL routing.
	Slug string `json:"slug,omitempty"`

	// ConversationRef is the adapter-scoped ref of the conversation the
	// agent is writing, as reported authoritatively by the agent hook
	// (ADR 0011). Opaque above the adapter: today's file-backed adapters
	// report their on-disk JSONL transcript path; other storage schemes
	// may report a different locator. It is the immutable Tool ID's
	// address; a change here is a rebind (/resume). Empty until the agent
	// reports it, or for unhooked adapters. The wire key stays
	// "conversation_file" for compatibility.
	ConversationRef string `json:"conversation_file,omitempty"`

	// Terminal size (updated by the runner whenever PTY is resized).
	TerminalCols uint16 `json:"terminal_cols,omitempty"`
	TerminalRows uint16 `json:"terminal_rows,omitempty"`

	// Transport
	SocketPath string `json:"socket_path"`

	// Build identity
	BinaryHash    string `json:"binary_hash,omitempty"`
	RunnerVersion string `json:"runner_version,omitempty"`

	// reservation tracks a semantic prompt gmux has delivered (or is
	// delivering) whose turn has not been observed yet (ADR 0027; see
	// AdmitAction). It lives here, next to Status and under the same mutex,
	// because that mutex is the runner's ONE ordering mechanism for turn
	// state: every authoritative status writer in the runner — agent hooks,
	// OSC 133 prompt marks, PUT /status, the launch/exit lifetime turn —
	// goes through SetStatus/CloseTurnFrame, so putting the semantic layer's
	// check-and-reserve in the same critical section makes it atomic
	// against all of them with no second lock to invert.
	//
	// activeEdges counts inactive→active transitions. A *transition* is the
	// only thing that can be evidence that a delivered prompt started a
	// turn: repeated Active=true writes (a hook that re-reports a running
	// turn, a script's `PUT /status` during one) say nothing new, and an
	// edge that happened before the delivery belongs to somebody else's
	// turn. Counting edges makes both distinctions expressible without turn
	// tokens or operation IDs.
	//
	// Both are unexported and never serialized: runner-generation-local
	// state about an in-flight delivery, not part of the session's document.
	reservation reservation
	activeEdges uint64

	// turnFrame is the adapter-asserted turn record relayed to /events
	// subscribers (see turnframe.go). Like reservation it lives under this
	// mutex because that mutex is the runner's one ordering mechanism for turn
	// state: the frame and the status write that closes a turn must be atomic
	// and ordered against each other, and against every other status writer.
	// Unexported and never serialized into the session document: it is live
	// truth, not row state.
	turnFrame *TurnFrame
	frameSeq  uint64

	// SSE subscribers (not serialized)
	subs []chan Event

	// Throttle for activity events — at most one per interval.
	lastActivity time.Time
}

// Event is sent over SSE to /events subscribers.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Config for creating a new session state.
type Config struct {
	ID              string
	Command         []string
	Cwd             string
	Adapter         string
	SocketPath      string
	BinaryHash      string
	RunnerVersion   string
	WorkspaceRoot   string
	Remotes         map[string]string
	ParentSessionID string
}

// New creates a new session state.
func New(cfg Config) *State {
	return &State{
		ID:              cfg.ID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		Command:         cfg.Command,
		Cwd:             cfg.Cwd,
		Adapter:         cfg.Adapter,
		WorkspaceRoot:   cfg.WorkspaceRoot,
		Remotes:         cfg.Remotes,
		ParentSessionID: cfg.ParentSessionID,
		SocketPath:      cfg.SocketPath,
		BinaryHash:      cfg.BinaryHash,
		RunnerVersion:   cfg.RunnerVersion,
		Alive:           false,
	}
}

// Title returns the resolved display title: adapter > shell > command basename.
func (s *State) Title() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.titleLocked()
}

func (s *State) titleLocked() string {
	if s.AdapterTitle != "" {
		return s.AdapterTitle
	}
	if s.ShellTitle != "" {
		return s.ShellTitle
	}
	return commandBasename(s.Command)
}

func commandBasename(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	display := make([]string, len(cmd))
	copy(display, cmd)
	if strings.Contains(display[0], "/") {
		display[0] = filepath.Base(display[0])
	}
	return strings.Join(display, " ")
}

// SetRunning marks the session as alive with the given PID.
func (s *State) SetRunning(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Alive = true
	s.Pid = pid
	s.StartedAt = time.Now().UTC().Format(time.RFC3339)
}

// SetExited marks the session as dead with the given exit code.
func (s *State) SetExited(exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setExitedLocked(exitCode, false, nil)
}

// SetExitedUnreadResult publishes process completion and its fresh result token
// in one event. The daemon therefore decides parent suppression before it can
// deliver attention for that same completion.
func (s *State) SetExitedUnreadResult(exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setExitedLocked(exitCode, true, nil)
}

// SetLifetimeExitedUnreadResult atomically closes the synthetic lifetime turn
// and publishes its process exit with one fresh result generation. Ordinary
// OSC prompt-cycle closes continue to use status events and are unaffected.
func (s *State) SetLifetimeExitedUnreadResult(status *adapter.Status, exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
	s.setExitedLocked(exitCode, true, status)
}

func (s *State) setExitedLocked(exitCode int, unread bool, status *adapter.Status) {
	if unread {
		s.markUnreadResultStateLocked()
	}
	s.Alive = false
	s.ExitCode = &exitCode
	s.ExitedAt = time.Now().UTC().Format(time.RFC3339)
	data := map[string]any{"exit_code": exitCode, "exited_at": s.ExitedAt}
	if status != nil {
		data["active"] = status.Active
		data["error"] = status.Error
		data["interrupted"] = status.Interrupted
	}
	if unread {
		data["unread"] = true
		data["unread_token"] = s.UnreadToken
	}
	s.emit(Event{Type: "exit", Data: data})
}

// SetUnread marks the session as having unseen output (or clears it).
func (s *State) SetUnread(unread bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setUnreadLocked(unread)
}

func (s *State) setUnreadLocked(unread bool) {
	if s.Unread == unread {
		return
	}
	if unread {
		s.markUnreadResultLocked()
		return
	}
	s.Unread = false
	s.emit(Event{Type: "meta", Data: map[string]any{"unread": false, "unread_token": s.UnreadToken}})
}

// MarkUnreadResult records a newly completed result even when an older result
// is still unread. Every completion receives a fresh opaque identity.
func (s *State) MarkUnreadResult() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markUnreadResultLocked()
}

func (s *State) markUnreadResultLocked() {
	s.markUnreadResultStateLocked()
	s.emit(Event{Type: "meta", Data: map[string]any{"unread": true, "unread_token": s.UnreadToken}})
}

// markUnreadResultStateLocked advances the state without publishing a separate
// event. Completion edges use it when unread must be fused with status/exit.
func (s *State) markUnreadResultStateLocked() {
	s.Unread = true
	s.UnreadToken = newUnreadToken()
}

// AcknowledgeUnread clears only the unread result generation the caller
// actually observed. A delayed acknowledgement for N can never erase N+1.
func (s *State) AcknowledgeUnread(expected string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.UnreadToken != expected {
		return false
	}
	s.setUnreadLocked(false)
	return true
}

func newUnreadToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
}

// activityThrottle is the minimum interval between activity events.
const activityThrottle = 2 * time.Second

// EmitActivity sends a lightweight "activity" event to signal that
// the terminal produced output. Throttled to at most once per 2s.
// This is not stored state — it's a transient signal for the frontend.
func (s *State) EmitActivity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.lastActivity) < activityThrottle {
		return
	}
	s.lastActivity = now
	s.emit(Event{Type: "activity", Data: nil})
}

// SetStatus updates the application status (from adapter or child).
func (s *State) SetStatus(status *adapter.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatusLocked(status)
	s.emit(Event{Type: "status", Data: status})
}

// SetStatusUnreadResult fuses a terminal status edge with the unread generation
// it completed. A subscriber can observe both or neither, never an attention
// fact before direct-parent suppression has been decided for the completion.
func (s *State) SetStatusUnreadResult(status *adapter.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markUnreadResultStateLocked()
	s.setStatusLocked(status)
	unread := true
	token := s.UnreadToken
	s.emit(Event{Type: "status", Data: turnEdge{Status: status, Unread: &unread, UnreadToken: &token}})
}

func (s *State) setStatusLocked(status *adapter.Status) {
	prev := s.Status
	s.Status = status
	s.noteStatusWriteLocked(prev, status)
}

// SetAdapterTitle sets the high-priority title from the adapter (agent hook / conversation file).
func (s *State) SetAdapterTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AdapterTitle == title {
		return
	}
	s.AdapterTitle = title
	s.emitMetaLocked()
}

// SetShellTitle sets the terminal/OSC title, used when no adapter title is set.
func (s *State) SetShellTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ShellTitle == title {
		return
	}
	s.ShellTitle = title
	s.emitMetaLocked()
}

// SetSlug sets the URL-safe session identifier, emitting a meta event only
// when it changes. Use on same-conversation refreshes, where the runner's
// state and the daemon's store are known to agree.
func (s *State) SetSlug(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Slug == slug {
		return
	}
	s.Slug = slug
	s.emit(Event{Type: "meta", Data: map[string]string{"slug": slug}})
}

// BindSlug sets the slug on an authoritative session bind and ALWAYS emits,
// even when the value is unchanged. On a re-register the daemon may have
// preserved a stale slug that diverges from this (fresh, possibly empty)
// runner state; a dedup'd SetSlug would then never tell the daemon to
// converge. Re-binds (switch/new/resume/fork) are infrequent, so the extra
// event is cheap. See handleHookEvent's session case.
func (s *State) BindSlug(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Slug = slug
	s.emit(Event{Type: "meta", Data: map[string]string{"slug": slug}})
}

// ConversationRefSnapshot returns the held conversation ref, for replay to a
// newly-connected /events subscriber so a reconnecting daemon re-learns
// attribution without persisted state.
func (s *State) ConversationRefSnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ConversationRef
}

// SlugSnapshot returns the current URL-safe slug under lock.
func (s *State) SlugSnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Slug
}

// StatusSnapshot returns a copy of the current status (nil if unset), safe to
// read from another goroutine while the runner concurrently updates state.
// Also replayed to every (re)connecting /events subscriber: status emitted
// before the daemon subscribed (the launch-time Active=true of the default
// turn model, or a turn started during a daemon restart) must not be lost.
func (s *State) StatusSnapshot() *adapter.Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Status == nil {
		return nil
	}
	cp := *s.Status
	return &cp
}

// TurnRequirement is the activity precondition a semantic agent action
// requires of the agent (ADR 0027): a plain prompt needs an idle agent, a
// steer needs a running turn, a follow-up needs neither.
type TurnRequirement int

const (
	RequireAny TurnRequirement = iota
	RequireInactive
	RequireActive
)

// ReservePolicy says whether a successful delivery should reserve the turn it
// is expected to produce. See AdmitAction.
type ReservePolicy int

const (
	// ReserveNever: the action joins a turn that is already running (a
	// steer) or starts none at all (an interrupt), so there is no future
	// turn to reserve.
	ReserveNever ReservePolicy = iota
	// ReserveIfInactive: a submit-now reserves only when it was delivered
	// to an idle agent, because only then is it the thing that starts the
	// next turn.
	ReserveIfInactive
	// ReserveAlways: a queued submit (send-after-turn) reserves the future
	// turn even though the agent is busy now — the queued text will run
	// when the current turn ends.
	ReserveAlways
)

// Admission is the verdict of AdmitAction.
type Admission int

const (
	Admitted Admission = iota
	// RefusedActive: the caller required an idle agent; a turn is running.
	RefusedActive
	// RefusedInactive: the caller required a running turn; none is.
	RefusedInactive
	// RefusedPending: no turn is running, but gmux already delivered a
	// prompt whose turn has not been observed yet. Admitting a second one
	// would duplicate input into the same composer.
	RefusedPending
)

// reservation is the generation-local phase of one delivered semantic prompt.
//
// Phases:
//
//	none                      held=false
//	unconfirmed (in flight)   held=true, inFlight=true   — committed, transport running
//	awaiting evidence         held=true, inFlight=false  — delivered, no qualifying edge yet
//
// A reservation is never released by the passage of time, and never by an
// inactive status write: neither is evidence that the agent consumed the text
// gmux delivered. It is released by a qualifying active edge — one that
// happens strictly after the reservation was committed (sinceEdges) — or by the
// delivering request itself when its transport fails.
type reservation struct {
	held     bool
	inFlight bool
	// sinceEdges is the activeEdges count at commit time. Only edges beyond
	// it can resolve this reservation, which is what stops an active edge
	// that predates the transport (e.g. an unrelated turn that started AND
	// finished while this request was queued) from being consumed as
	// evidence for it.
	sinceEdges uint64
}

// AdmitAction is the semantic layer's transport-start boundary: it evaluates a
// semantic action's activity requirement against the authoritative status AND
// the outstanding delivery reservation and, when it admits, commits the
// reservation the policy asks for. It reports whether it reserved, so the
// delivering request can confirm or undo exactly what it did
// (ConfirmDelivery / ClearReservation).
//
// Callers must call it IMMEDIATELY before the transport write, with nothing in
// between. That is what makes the requirement check meaningful: the check, the
// commit and every status write in the runner share one critical section, so no
// transition can land between the check and the decision, and a request that
// sat in a queue for a second is decided on the state it is actually delivering
// into rather than on the state it saw when it arrived.
//
// Activity is exactly Status.Active: an errored-but-still-running turn is
// active, while a terminal error, an interruption and "nothing reported yet"
// are all inactive.
func (s *State) AdmitAction(req TurnRequirement, pol ReservePolicy) (verdict Admission, reserved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.Status != nil && s.Status.Active
	switch req {
	case RequireActive:
		if !active {
			return RefusedInactive, false
		}
	case RequireInactive:
		if active {
			return RefusedActive, false
		}
		// An unresolved delivery of ours means a prompt is sitting in the
		// agent's composer (or in flight to it) that the agent has not
		// reported a turn for yet. It is NOT idle in the sense the caller
		// means, even though the status says so.
		if s.reservation.held {
			return RefusedPending, false
		}
	}
	switch pol {
	case ReserveAlways:
		s.reserveLocked()
		return Admitted, true
	case ReserveIfInactive:
		if !active {
			s.reserveLocked()
			return Admitted, true
		}
	}
	return Admitted, false
}

// reserveLocked commits an unconfirmed reservation whose evidence watermark is
// the current edge count: only an inactive→active transition from here on can
// resolve it. Caller must hold s.mu.
func (s *State) reserveLocked() {
	s.reservation = reservation{held: true, inFlight: true, sinceEdges: s.activeEdges}
}

// ConfirmDelivery ends the in-flight phase of the reservation this caller took,
// after its transport wrote the whole payload. It releases the reservation only
// if a qualifying active edge was already observed while the write was in
// flight; otherwise the reservation stays held, waiting for one.
//
// Why the phase exists at all: an agent can start its turn — and its hook can
// report it — before the runner's write call returns, so the evidence and the
// delivery genuinely race. Resolving such an edge immediately would discard the
// reservation before anyone knows whether the write succeeded, and a write that
// then failed would leave the session with no reservation and no delivery.
// Recording the edge and consulting it here keeps both orders correct.
//
// An edge that raced the write is ASSUMED to belong to this delivery. Without a
// turn token it is causally ambiguous: a human typing at the terminal (or any
// raw writer) could have started that turn instead, and consuming their edge
// releases our reservation early. That ambiguity is resolved toward
// availability on purpose — the alternative wedges the common successful case,
// where our own prompt is exactly what started the turn, for the rest of the
// runner generation. Concurrent raw/human input is outside what ADR 0027's
// semantic layer guarantees anything against.
func (s *State) ConfirmDelivery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reservation.held {
		return
	}
	s.reservation.inFlight = false
	if s.activeEdges > s.reservation.sinceEdges {
		s.reservation = reservation{}
	}
}

// ClearReservation undoes a reservation whose delivery did not happen or was
// truncated. Only the caller that took the reservation may call it, and only
// while it still holds the runner's delivery slot — otherwise it could erase
// somebody else's.
func (s *State) ClearReservation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservation = reservation{}
}

// AbandonInFlightReservation drops a reservation that is still in flight, and
// only then. It is the safety net for a delivering request that never reaches
// ConfirmDelivery or ClearReservation — a panic in the transport, or any other
// unexpected unwind: an orphaned in-flight reservation would refuse every later
// prompt for the rest of the runner generation, with no request left to resolve
// it.
//
// Deliberately conditional on the in-flight phase, so it is a no-op after a
// normal outcome: ConfirmDelivery ends that phase (leaving a legitimately held
// reservation that is still waiting for its turn), and ClearReservation empties
// it. Like the other two, it may only be called by the request that took the
// reservation, while it still holds the delivery slot.
func (s *State) AbandonInFlightReservation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservation.held && s.reservation.inFlight {
		s.reservation = reservation{}
	}
}

// ReservationHeld reports whether a delivered (or in-flight) prompt is still
// waiting for its turn to be observed. Diagnostics and tests only — admission
// decisions must use AdmitAction, which reads it in the same breath as the
// status.
func (s *State) ReservationHeld() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reservation.held
}

// noteStatusWriteLocked records what an authoritative status write means for
// the delivery reservation. Caller must hold s.mu and must pass the status that
// was in place before the write, because the distinction that matters is a
// TRANSITION, not a value.
//
// Only an inactive→active edge counts:
//
//   - a repeated Active=true write (a hook re-reporting a running turn, a
//     script's `PUT /status` mid-turn, a follow-up's own turn still going) is
//     not new information and must not resolve a reservation for a turn that
//     has not started;
//   - an inactive write — a turn end, an idle `PUT /status`, a cleared status,
//     an interruption — is not evidence that the prompt gmux delivered was
//     consumed, so it must not re-open the door to a second one;
//   - and an edge that predates the reservation (sinceEdges) belongs to
//     somebody else's turn.
//
// There is deliberately NO timeout: a delivered prompt that never produces a
// turn stays unresolved for the rest of the runner generation, because nothing
// about the passage of time proves the agent did not receive it. Duplicating a
// prompt is worse than refusing one, and raw input remains available as the
// unconditional escape hatch.
//
// While the delivering write is still in flight the edge is only *recorded*
// (the counter advances); ConfirmDelivery consults it once the write is known
// to have completed. See ConfirmDelivery.
func (s *State) noteStatusWriteLocked(prev, next *adapter.Status) {
	prevActive := prev != nil && prev.Active
	nextActive := next != nil && next.Active
	if !nextActive || prevActive {
		return // not an inactive→active edge
	}
	s.activeEdges++
	if s.reservation.held && !s.reservation.inFlight && s.activeEdges > s.reservation.sinceEdges {
		s.reservation = reservation{}
	}
}

// UnreadSnapshot returns the current unread flag under lock.
func (s *State) UnreadSnapshot() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Unread
}

// SetConversationRef records the agent's current conversation ref as reported
// by the extension. Emits a conversation_file event (legacy wire name; the
// payload's "path" key carries the ref) only when the ref changes, so the
// daemon sees first-attribution and rebind (/resume) but not every write.
func (s *State) SetConversationRef(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref == "" || ref == s.ConversationRef {
		return
	}
	s.ConversationRef = ref
	s.emit(Event{Type: "conversation_file", Data: map[string]string{"path": ref}})
}

// SetSubtitle updates the display subtitle.
func (s *State) SetSubtitle(subtitle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Subtitle = subtitle
	s.emitMetaLocked()
}

// SetTerminalSize records the current PTY dimensions and emits a terminal_resize
// event so gmuxd discovery can update the store without relying on the proxy.
func (s *State) SetTerminalSize(cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TerminalCols = cols
	s.TerminalRows = rows
	s.emit(Event{Type: "terminal_resize", Data: map[string]uint16{
		"cols": cols,
		"rows": rows,
	}})
}

func (s *State) emitMetaLocked() {
	data := map[string]string{
		"title":       s.titleLocked(),
		"shell_title": s.ShellTitle,
	}
	if s.AdapterTitle != "" {
		data["adapter_title"] = s.AdapterTitle
	}
	if s.Subtitle != "" {
		data["subtitle"] = s.Subtitle
	}
	s.emit(Event{Type: "meta", Data: data})
}

// MarshalJSON produces JSON with a computed "title" field.
func (s *State) MarshalJSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type Alias State
	return json.Marshal(&struct {
		Title string `json:"title"`
		*Alias
	}{
		Title: s.titleLocked(),
		Alias: (*Alias)(s),
	})
}

// ExitSnapshot returns the runner-owned exit metadata for replay to a
// reconnecting subscriber. A nil code means the session has not exited.
func (s *State) ExitSnapshot() (*int, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ExitCode == nil {
		return nil, ""
	}
	code := *s.ExitCode
	return &code, s.ExitedAt
}

// Subscribe returns a channel that receives events.
func (s *State) Subscribe() chan Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, 16)
	s.subs = append(s.subs, ch)
	return ch
}

// Unsubscribe removes a subscription channel.
func (s *State) Unsubscribe(ch chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sub := range s.subs {
		if sub == ch {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (s *State) emit(e Event) {
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
