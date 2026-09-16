package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

var errInvalidProjectJSON = errors.New("invalid project JSON")

func decodeProjectState(body []byte) (projects.State, error) {
	var state projects.State
	if err := json.Unmarshal(body, &state); err != nil {
		return projects.State{}, fmt.Errorf("%w: %v", errInvalidProjectJSON, err)
	}
	if err := state.Validate(); err != nil {
		return projects.State{}, err
	}
	return state, nil
}

type fanoutMessage struct {
	Frames         wire.Frames
	SessionsEncode *sessionEncodeMemo // non-nil iff Frames.Sessions is
	ActivityID     string
	ProjectsUpdate bool
}

type sseFanout struct {
	mu       sync.Mutex
	epoch    uint64 // protocol-3 epoch source; advanced under mu so per-subscriber delivery order matches epoch order
	sessions *wire.SessionsPayload
	world    *wire.WorldPayload
	subs     map[chan fanoutMessage]struct{}

	// PROTO (3.0). bootID makes epochs comparable across daemon restarts;
	// ring tracks per-view changesets; current is the memo of the state the
	// last broadcast left behind (what a new subscriber or a scope baseline
	// starts from); conns holds per-connection scope registrations, keyed by
	// the connection id handed out in stream.hello.
	bootID      string
	ring        *deltaRing
	ownedFilter func(string) bool
	current     *sessionEncodeMemo
	conns       map[string]*connScopes
}

// connScopes is one SSE connection's registered scopes and, per scope, the
// epoch whose value the connection already holds.
type connScopes struct {
	lastSent map[string]uint64
}

// sessionEncodeMemo encodes one broadcast sessions payload at most once per
// (view × protocol) and shares the result across every SSE subscriber.
//
// The memo owns an ANNOTATED copy of the payload (descendant counts stamped,
// family index built) computed once per broadcast; every view — full or roots,
// browser or peer — and every scope's rows are cut from that one copy, so a
// root's counts, its children page and a scoped delta all describe the same
// epoch.
type sessionEncodeMemo struct {
	epoch   uint64
	payload *wire.SessionsPayload

	mu        sync.Mutex
	annotated []wire.Session
	family    *wire.FamilyIndex
	views     map[int]*wire.SessionsPayload
	index     map[int]map[string]int
	proto2    map[int][]byte
	proto3    map[int][]sessionstream.Event
	// isLocalPeer is captured at first use: it is the same predicate for
	// every subscriber of one daemon.
	isLocalPeer func(string) bool
}

func newSessionEncodeMemo(epoch uint64, payload *wire.SessionsPayload) *sessionEncodeMemo {
	if payload == nil {
		return nil
	}
	return &sessionEncodeMemo{epoch: epoch, payload: payload, views: map[int]*wire.SessionsPayload{}, index: map[int]map[string]int{}, proto2: map[int][]byte{}, proto3: map[int][]sessionstream.Event{}}
}

// SetLocalPeer installs the ownership predicate used by the peer views.
func (m *sessionEncodeMemo) SetLocalPeer(isLocalPeer func(string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isLocalPeer == nil {
		m.isLocalPeer = isLocalPeer
	}
}

func (m *sessionEncodeMemo) annotateLocked() {
	if m.annotated != nil {
		return
	}
	rows := make([]wire.Session, len(m.payload.Sessions))
	copy(rows, m.payload.Sessions)
	m.family = wire.AnnotateDescendantCounts(rows)
	m.annotated = rows
}

// Annotated returns the full payload with descendant counts stamped, and its
// family index. Callers must not mutate either.
func (m *sessionEncodeMemo) Annotated() ([]wire.Session, *wire.FamilyIndex) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.annotateLocked()
	return m.annotated, m.family
}

func (m *sessionEncodeMemo) viewLocked(class int) *wire.SessionsPayload {
	if p, ok := m.views[class]; ok {
		return p
	}
	m.annotateLocked()
	rows := m.annotated
	switch class {
	case deltaClassOwned, deltaClassRootsOwned:
		rows = wire.SessionsPayload{Sessions: rows}.FilterOwned(m.isLocalPeer).Sessions
	}
	switch class {
	case deltaClassRoots, deltaClassRootsOwned:
		rows = wire.RootsView(rows, m.family)
	}
	p := &wire.SessionsPayload{Sessions: rows}
	m.views[class] = p
	return p
}

// ViewRows is the row list one view class sees at this epoch.
func (m *sessionEncodeMemo) ViewRows(class int) []wire.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(class).Sessions
}

// ScopeRows is the row list a dynamic scope sees at this epoch: the direct
// family children of a session, or one session.
func (m *sessionEncodeMemo) ScopeRows(key string) []wire.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.annotateLocked()
	switch {
	case strings.HasPrefix(key, scopeChildrenPrefix):
		id := key[len(scopeChildrenPrefix):]
		page, ok := wire.ListChildren(m.annotated, m.family, id, false, "", 200)
		if !ok {
			return nil
		}
		// ListChildren pages; a scope wants every direct child. Walk pages.
		rows := append([]wire.Session(nil), page.Rows...)
		for page.NextCursor != "" {
			page, _ = wire.ListChildren(m.annotated, m.family, id, false, page.NextCursor, 200)
			rows = append(rows, page.Rows...)
		}
		return rows
	case strings.HasPrefix(key, scopeSessionPrefix):
		id := key[len(scopeSessionPrefix):]
		for i := range m.annotated {
			if m.annotated[i].ID == id {
				return m.annotated[i : i+1]
			}
		}
	}
	return nil
}

func (m *sessionEncodeMemo) indexLocked(class int) (*wire.SessionsPayload, map[string]int) {
	p := m.viewLocked(class)
	idx, ok := m.index[class]
	if !ok {
		idx = make(map[string]int, len(p.Sessions))
		for i, s := range p.Sessions {
			idx[s.ID] = i
		}
		m.index[class] = idx
	}
	return p, idx
}

// DeltaRows resolves touched ids against this broadcast's view for one class.
// Present ids come back as upserts carrying the row's value AT THIS EPOCH;
// absent ids come back as removals — which is how a row that was filtered out
// (ownership flip, dismissal, became a child) reaches the client as a
// removal rather than going stale.
func (m *sessionEncodeMemo) DeltaRows(class int, touched []string) ([]wire.Session, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, idx := m.indexLocked(class)
	return resolveTouched(p.Sessions, idx, touched)
}

// ScopeDeltaRows is DeltaRows for a dynamic scope.
func (m *sessionEncodeMemo) ScopeDeltaRows(key string, touched []string) ([]wire.Session, []string) {
	rows := m.ScopeRows(key)
	idx := make(map[string]int, len(rows))
	for i, s := range rows {
		idx[s.ID] = i
	}
	return resolveTouched(rows, idx, touched)
}

func resolveTouched(rows []wire.Session, idx map[string]int, touched []string) ([]wire.Session, []string) {
	upsert := make([]wire.Session, 0, len(touched))
	remove := make([]string, 0, 8)
	for _, id := range touched {
		if i, present := idx[id]; present {
			upsert = append(upsert, rows[i])
			continue
		}
		remove = append(remove, id)
	}
	return upsert, remove
}

// Proto2 returns the marshaled snapshot.sessions body for the subscriber's
// view class, encoding it on first use.
func (m *sessionEncodeMemo) Proto2(class int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if data, ok := m.proto2[class]; ok {
		return data, nil
	}
	data, err := json.Marshal(m.viewLocked(class))
	if err != nil {
		return nil, err
	}
	m.proto2[class] = data
	return data, nil
}

// Proto3 returns the protocol-3 transaction events for the subscriber's
// view class, encoding them on first use.
func (m *sessionEncodeMemo) Proto3(class int) ([]sessionstream.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if events, ok := m.proto3[class]; ok {
		return events, nil
	}
	p := m.viewLocked(class)
	events, err := sessionstream.Encode(m.epoch, p.Sessions, func(s wire.Session) string { return s.ID })
	if err != nil {
		return nil, err
	}
	m.proto3[class] = events
	return events, nil
}

func newSSEFanout() *sseFanout {
	return &sseFanout{subs: make(map[chan fanoutMessage]struct{}), bootID: newBootID(), ring: newDeltaRing(), conns: map[string]*connScopes{}}
}

func newBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// SetOwnershipFilter installs the ?as=peer membership predicate so the peer
// views can be cut. Called once at startup, before any broadcast.
func (f *sseFanout) SetOwnershipFilter(isLocalPeer func(string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownedFilter = isLocalPeer
}

func (f *sseFanout) BootID() string { return f.bootID }

// Epoch is the current fanout epoch.
func (f *sseFanout) Epoch() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch
}

// CurrentMemo is the memo of the state the last broadcast left behind (nil
// before the first sessions broadcast). Read paths that must agree with what
// subscribers were told at this epoch (children pages) cut from it.
func (f *sseFanout) CurrentMemo() (*sessionEncodeMemo, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.epoch
}

// TouchedSince is the delta chain lookup for a view class (see deltaRing).
func (f *sseFanout) TouchedSince(from, to uint64, class int) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ring == nil {
		return nil, false
	}
	return f.ring.TouchedSince(from, to, class)
}

// ScopeTouchedSince is the delta chain lookup for a dynamic scope.
func (f *sseFanout) ScopeTouchedSince(from, to uint64, key string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ring == nil {
		return nil, false
	}
	return f.ring.ScopeTouchedSince(from, to, key)
}

// DemandClass starts tracking a view class's changesets (idempotent, sticky).
func (f *sseFanout) DemandClass(class int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ring == nil {
		return
	}
	f.ring.DemandClass(class, f.current)
}

// RegisterConn hands a delta-capable connection its id. The returned release
// drops every scope the connection still holds.
func (f *sseFanout) RegisterConn() (string, func()) {
	id := newBootID()
	f.mu.Lock()
	f.conns[id] = &connScopes{lastSent: map[string]uint64{}}
	f.mu.Unlock()
	return id, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		cs, ok := f.conns[id]
		if !ok {
			return
		}
		if f.ring != nil {
			for key := range cs.lastSent {
				f.ring.ReleaseScope(key)
			}
		}
		delete(f.conns, id)
	}
}

// UpdateScopes applies one POST /v1/events/scopes request. The returned epoch
// is the fanout epoch the new scopes chain from: a client holding a page cut
// at an older epoch must refetch it. ok is false for an unknown connection.
func (f *sseFanout) UpdateScopes(conn string, add, remove []string) (epoch uint64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs, known := f.conns[conn]
	if !known {
		return 0, false
	}
	for _, key := range remove {
		if _, held := cs.lastSent[key]; !held {
			continue
		}
		delete(cs.lastSent, key)
		if f.ring != nil {
			f.ring.ReleaseScope(key)
		}
	}
	for _, key := range add {
		if !validScopeKey(key) {
			continue
		}
		if _, held := cs.lastSent[key]; held {
			continue
		}
		cs.lastSent[key] = f.epoch
		if f.ring != nil {
			f.ring.AddScope(key, f.current)
		}
	}
	return f.epoch, true
}

// ScopesOf snapshots a connection's scopes and their last-sent epochs.
func (f *sseFanout) ScopesOf(conn string) map[string]uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs, ok := f.conns[conn]
	if !ok {
		return nil
	}
	out := make(map[string]uint64, len(cs.lastSent))
	for k, v := range cs.lastSent {
		out[k] = v
	}
	return out
}

// AdvanceScope records that a connection now holds `epoch` for one scope.
// Ignored when the scope was released meanwhile.
func (f *sseFanout) AdvanceScope(conn, key string, epoch uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cs, ok := f.conns[conn]; ok {
		if _, held := cs.lastSent[key]; held {
			cs.lastSent[key] = epoch
		}
	}
}

// DisableDeltas is the kill switch: without a ring no chain can be offered,
// every subscriber falls back to today's full transaction, and the per-
// broadcast diff cost disappears.
func (f *sseFanout) DisableDeltas() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ring = nil
}

func (f *sseFanout) Current() wire.Frames {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentLocked()
}

func (f *sseFanout) currentLocked() wire.Frames {
	var out wire.Frames
	if f.sessions != nil {
		copy := *f.sessions
		copy.Sessions = copySlice(f.sessions.Sessions)
		out.Sessions = &copy
	}
	if f.world != nil {
		copy := *f.world
		copy.Projects = copySlice(f.world.Projects)
		copy.Peers = copySlice(f.world.Peers)
		copy.Launchers = copySlice(f.world.Launchers)
		if f.world.PeerProjects != nil {
			copy.PeerProjects = make(map[string][]peering.SpokeProject, len(f.world.PeerProjects))
			for k, v := range f.world.PeerProjects {
				copy.PeerProjects[k] = append([]peering.SpokeProject{}, v...)
			}
		}
		if f.world.PeerDiscovered != nil {
			copy.PeerDiscovered = make(map[string][]peering.SpokeDiscovered, len(f.world.PeerDiscovered))
			for k, v := range f.world.PeerDiscovered {
				copy.PeerDiscovered[k] = append([]peering.SpokeDiscovered{}, v...)
			}
		}
		if f.world.Health != nil {
			h := *f.world.Health
			copy.Health = &h
		}
		out.World = &copy
	}
	return out
}

func (f *sseFanout) newMemoLocked(epoch uint64, payload *wire.SessionsPayload) *sessionEncodeMemo {
	m := newSessionEncodeMemo(epoch, payload)
	if m != nil {
		m.isLocalPeer = f.ownedFilter
	}
	return m
}

func (f *sseFanout) Subscribe() (fanoutMessage, <-chan fanoutMessage, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan fanoutMessage, 32)
	f.subs[ch] = struct{}{}
	frames := f.currentLocked()
	f.epoch++
	// A subscribe epoch is NOT a broadcast: it publishes the state the last
	// broadcast left behind, so it appends no ring entry. Resuming from it is
	// still well defined — the union of entries strictly newer than it is
	// exactly what the subscriber missed. The initial memo shares the current
	// memo's annotation work when there is one.
	memo := f.newMemoLocked(f.epoch, frames.Sessions)
	if memo != nil && f.current != nil {
		memo.annotated, memo.family = f.current.annotated, f.current.family
		for k, v := range f.current.views {
			memo.views[k] = v
		}
	}
	initial := fanoutMessage{Frames: frames, SessionsEncode: memo}
	cancel := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[ch]; !ok {
			return
		}
		delete(f.subs, ch)
		close(ch)
	}
	return initial, ch, cancel
}

func (f *sseFanout) BroadcastFrames(frames wire.Frames) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if frames.Sessions != nil {
		copy := *frames.Sessions
		copy.Sessions = copySlice(frames.Sessions.Sessions)
		f.sessions = &copy
	}
	if frames.World != nil {
		copy := *frames.World
		copy.Projects = copySlice(frames.World.Projects)
		copy.Peers = copySlice(frames.World.Peers)
		copy.Launchers = copySlice(frames.World.Launchers)
		if frames.World.PeerProjects != nil {
			copy.PeerProjects = make(map[string][]peering.SpokeProject, len(frames.World.PeerProjects))
			for k, v := range frames.World.PeerProjects {
				copy.PeerProjects[k] = append([]peering.SpokeProject{}, v...)
			}
		}
		if frames.World.PeerDiscovered != nil {
			copy.PeerDiscovered = make(map[string][]peering.SpokeDiscovered, len(frames.World.PeerDiscovered))
			for k, v := range frames.World.PeerDiscovered {
				copy.PeerDiscovered[k] = append([]peering.SpokeDiscovered{}, v...)
			}
		}
		if frames.World.Health != nil {
			h := *frames.World.Health
			copy.Health = &h
		}
		f.world = &copy
	}
	f.epoch++
	var memo *sessionEncodeMemo
	if frames.Sessions != nil {
		memo = f.newMemoLocked(f.epoch, frames.Sessions)
		if f.ring != nil {
			f.ring.Record(f.epoch, memo)
		}
		f.current = memo
	} else if f.current != nil {
		// A world-only broadcast leaves the sessions view where it was; the
		// current memo simply advances its epoch label.
		// (Subscribers receive no sessions frame and keep their chain.)
	}
	msg := fanoutMessage{Frames: frames, SessionsEncode: memo, ProjectsUpdate: frames.World != nil}
	for ch := range f.subs {
		fanoutEnqueue(ch, msg)
	}
}

func (f *sseFanout) BroadcastActivity(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg := fanoutMessage{ActivityID: id}
	for ch := range f.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func fanoutEnqueue(ch chan fanoutMessage, msg fanoutMessage) {
	select {
	case ch <- msg:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- msg:
	default:
	}
}

func visibleSession(payload *wire.SessionsPayload, id string) (wire.Session, bool) {
	if payload == nil {
		return wire.Session{}, false
	}
	for _, s := range payload.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return wire.Session{}, false
}

func sessionLastActiveWire(s wire.Session) string {
	if s.LastOutputAt != "" {
		return s.LastOutputAt
	}
	return s.CreatedAt
}

func buildSessionInfosWire(payload *wire.SessionsPayload, isLocalPeer func(string) bool) []projects.SessionInfo {
	if payload == nil {
		return nil
	}
	infos := make([]projects.SessionInfo, 0, len(payload.Sessions))
	for _, s := range payload.Sessions {
		infos = append(infos, projects.SessionInfo{
			ID:            s.ID,
			Cwd:           s.Cwd,
			WorkspaceRoot: s.WorkspaceRoot,
			Remotes:       copyStringMap(s.Remotes),
			Host:          s.Peer,
			LocalHost:     s.Peer != "" && isLocalPeer != nil && isLocalPeer(s.Peer),
			Alive:         s.Alive,
			Resumable:     s.Resumable,
			LastActive:    sessionLastActiveWire(s),
		})
	}
	return infos
}

func projectStateFromWorld(world *wire.WorldPayload) projects.State {
	state := projects.State{Version: 4}
	if world == nil {
		return state
	}
	state.Items = make([]projects.Item, 0, len(world.Projects))
	for _, item := range world.Projects {
		p := projects.Item{Slug: item.Slug, Peer: item.Peer, Sessions: append([]string(nil), item.Sessions...), NodeID: item.NodeID}
		for _, rule := range item.Match {
			p.Match = append(p.Match, projects.MatchRule{Path: rule.Path, Remote: rule.Remote, Exact: rule.Exact})
		}
		state.Items = append(state.Items, p)
	}
	return state
}

// advertisingHostSessions returns native sessions and Local peers whose
// project assignment this host owns. Network peers advertise themselves.
func advertisingHostSessions(sessions []projects.SessionInfo) []projects.SessionInfo {
	owned := make([]projects.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if session.Host == "" || session.LocalHost {
			owned = append(owned, session)
		}
	}
	return owned
}

// projectDiscoveryData builds the GET /v1/projects payload. Discovery and its
// active count deliberately share one ownership-scoped input so the endpoint
// cannot re-advertise a network peer's sessions under this spoke's identity.
func projectDiscoveryData(frames wire.Frames, isLocalPeer func(string) bool) map[string]any {
	state := projectStateFromWorld(frames.World)
	infos := buildSessionInfosWire(frames.Sessions, isLocalPeer)
	owned := advertisingHostSessions(infos)
	return map[string]any{
		"configured":             state.Items,
		"discovered":             state.Discovered(owned),
		"unmatched_active_count": state.UnmatchedActiveCount(owned),
	}
}

func projectSpecsFromState(state projects.State) []centralstore.ProjectEntrySpec {
	specs := make([]centralstore.ProjectEntrySpec, 0, len(state.Items))
	for _, item := range state.Items {
		if item.Peer != "" {
			specs = append(specs, centralstore.ProjectEntrySpec{Reference: &centralstore.ProjectReference{PeerKey: centralstore.PeerKey(item.Peer), Slug: item.Slug, NodeID: item.NodeID}})
			continue
		}
		spec := centralstore.ProjectEntrySpec{Owned: &centralstore.OwnedProjectSpec{Slug: item.Slug}}
		for _, rule := range item.Match {
			spec.Owned.Rules = append(spec.Owned.Rules, centralstore.MatchRule{Path: rule.Path, Remote: rule.Remote, Exact: rule.Exact})
		}
		specs = append(specs, spec)
	}
	return specs
}

func centralSessionToLegacy(row centralstore.Session) compatSession {
	var status *compatStatus
	if row.StatusReported {
		status = &compatStatus{Active: row.Active, Error: row.Error, Interrupted: row.Interrupted}
	}
	return compatSession{
		ID:              string(row.ID),
		CreatedAt:       fmtMillis(row.CreatedAt),
		Command:         append([]string(nil), row.Command...),
		Cwd:             row.CWD,
		Adapter:         row.Adapter,
		WorkspaceRoot:   row.WorkspaceRoot,
		Remotes:         copyStringMap(row.Remotes),
		Alive:           false,
		ExitCode:        row.ExitCode,
		StartedAt:       fmtMillisPtr(row.StartedAt),
		ExitedAt:        fmtMillisPtr(row.ExitedAt),
		Title:           row.Title,
		Subtitle:        row.Subtitle,
		Status:          status,
		Unread:          row.Unread,
		UnreadToken:     row.UnreadToken,
		TerminalCols:    uint16Value(row.TerminalCols),
		TerminalRows:    uint16Value(row.TerminalRows),
		Slug:            row.Slug,
		ConversationRef: row.ConversationRef,
		LastOutputAt:    fmtMillisPtr(row.LastActivityAt),
	}
}

func legacySessionFromWire(s wire.Session) compatSession {
	var status *compatStatus
	if s.Status != nil {
		status = &compatStatus{Active: s.Status.Active, Error: s.Status.Error, Interrupted: s.Status.Interrupted}
	}
	return compatSession{
		ID:              s.ID,
		Peer:            s.Peer,
		CreatedAt:       s.CreatedAt,
		Command:         append([]string(nil), s.Command...),
		Cwd:             s.Cwd,
		Adapter:         s.Adapter,
		WorkspaceRoot:   s.WorkspaceRoot,
		Remotes:         copyStringMap(s.Remotes),
		ParentSessionID: s.ParentSessionID,
		Alive:           s.Alive,
		Pid:             s.Pid,
		ExitCode:        s.ExitCode,
		StartedAt:       s.StartedAt,
		ExitedAt:        s.ExitedAt,
		Title:           s.Title,
		Subtitle:        s.Subtitle,
		Status:          status,
		Unread:          s.Unread,
		UnreadToken:     s.UnreadToken,
		Resumable:       s.Resumable,
		SocketPath:      s.SocketPath,
		TerminalCols:    s.TerminalCols,
		TerminalRows:    s.TerminalRows,
		Slug:            s.Slug,
		ConversationRef: s.ConversationRef,
		RunnerVersion:   s.RunnerVersion,
		BinaryHash:      s.BinaryHash,
		ProjectSlug:     s.ProjectSlug,
		ProjectIndex:    s.ProjectIndex,
		LastOutputAt:    s.LastOutputAt,
	}
}

func legacySessionFromOutcome(o centralstore.Session, alive bool) compatSession {
	s := centralSessionToLegacy(o)
	s.Alive = alive
	return s
}

func fmtMillis(v centralstore.UnixMillis) string {
	return time.UnixMilli(int64(v)).UTC().Format(time.RFC3339)
}

func fmtMillisPtr(v *centralstore.UnixMillis) string {
	if v == nil {
		return ""
	}
	return fmtMillis(*v)
}

func uint16Value(v *uint16) uint16 {
	if v == nil {
		return 0
	}
	return *v
}

func ownedProjectStateFromCatalog(catalog centralstore.ProjectCatalog) *projects.State {
	state := &projects.State{Version: 4}
	for _, entry := range catalog {
		item := projects.Item{Slug: entry.Slug, Peer: string(entry.PeerKey), NodeID: entry.NodeID}
		for _, rule := range entry.Rules {
			item.Match = append(item.Match, projects.MatchRule{Path: rule.Path, Remote: rule.Remote, Exact: rule.Exact})
		}
		state.Items = append(state.Items, item)
	}
	return state
}

func resolveResumeDirCentral(ctx context.Context, st *centralstore.Store, row centralstore.Session) (string, bool, error) {
	cwd := projects.NormalizePath(row.CWD)
	canonical := ""
	snap, err := st.ReadSnapshot(ctx, centralstore.SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	if err != nil {
		return "", false, err
	}
	state := ownedProjectStateFromCatalog(snap.Projects)
	projectSlug := ""
	for _, view := range snap.Sessions {
		if view.ID == row.ID && view.Placement != nil {
			projectSlug = view.Placement.ProjectSlug
			break
		}
	}
	canonical = state.CanonicalDirForSession(projectSlug, projects.MatchParams{Cwd: row.CWD, WorkspaceRoot: row.WorkspaceRoot, Remotes: row.Remotes})
	dir, idx := projects.ResolveLaunchDir(projects.IsDir, cwd, canonical, os.Getenv("HOME"))
	if dir == "" {
		return "", false, nil
	}
	return dir, idx > 0, nil
}

func reorderPayloads(ctx context.Context, st *centralstore.Store) (*central.SessionsPayload, *central.ProjectsPayload, error) {
	snap, err := st.ReadSnapshot(ctx, centralstore.SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	if err != nil {
		return nil, nil, err
	}
	sp := &central.SessionsPayload{Sessions: make([]central.SessionRow, 0, len(snap.Sessions))}
	for _, row := range snap.Sessions {
		sp.Sessions = append(sp.Sessions, central.SessionRow{SessionView: row})
	}
	wp := &central.ProjectsPayload{Projects: snap.Projects, LocalPeerPlacements: make([]central.LocalPeerPlacementRow, 0, len(snap.LocalPeerPlacements))}
	for _, row := range snap.LocalPeerPlacements {
		wp.LocalPeerPlacements = append(wp.LocalPeerPlacements, central.LocalPeerPlacementRow{LocalPeerPlacementView: row})
	}
	return sp, wp, nil
}

func registryRuntime(reg *sessioncoord.Registry, id centralstore.SessionID) (sessioncoord.Runtime, bool) {
	for _, runtime := range reg.Snapshot() {
		if runtime.SessionID == id {
			return runtime, true
		}
	}
	return sessioncoord.Runtime{}, false
}

// terminalWSEndpoint resolves the runner backend for the terminal
// WebSocket (/ws/{id}). The drive-mode boundary is answered from the
// AUTHORITATIVE store, not the composed fanout snapshot: immediately after
// an ACP registration the snapshot can lag (or be empty), and falling
// through to the live registry endpoint would attempt a terminal proxy
// against a runner that has no PTY (ADR 0033). A store failure refuses
// conservatively: a backend must not be resolved when the session's mode
// cannot be established.
func terminalWSEndpoint(ctx context.Context, st *centralstore.Store, reg *sessioncoord.Registry, fanout *sseFanout, sessionID string) (string, error) {
	sid := centralstore.SessionID(sessionID)
	row, found, err := st.Session(ctx, sid)
	if err != nil {
		return "", fmt.Errorf("session %s drive mode could not be verified: %v", sessionID, err)
	}
	if found && row.DriveMode == centralstore.DriveModeACP {
		return "", fmt.Errorf("session %s is an ACP session; there is no terminal to attach", sessionID)
	}
	if e, ok := registryRuntime(reg, sid); ok {
		return e.Endpoint, nil
	}
	if _, ok := visibleSession(fanout.Current().Sessions, sessionID); ok {
		return "", fmt.Errorf("session %s has no socket", sessionID)
	}
	return "", fmt.Errorf("session %s not found", sessionID)
}

func sessionTreeRows(ctx context.Context, st *centralstore.Store, root centralstore.SessionID) ([]centralstore.Session, error) {
	rows, err := st.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	return projectSessionTreeRows(rows, root)
}

func projectSessionTreeRows(rows []centralstore.Session, root centralstore.SessionID) ([]centralstore.Session, error) {
	byID := make(map[centralstore.SessionID]int, len(rows))
	byParent := make(map[centralstore.SessionID][]centralstore.SessionID)
	for i := range rows {
		row := &rows[i]
		byID[row.ID] = i
		if row.ParentSessionID != nil {
			byParent[*row.ParentSessionID] = append(byParent[*row.ParentSessionID], row.ID)
		}
	}
	if _, present := byID[root]; !present {
		return nil, fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, root)
	}
	for _, kids := range byParent {
		sort.Slice(kids, func(i, j int) bool { return kids[i] < kids[j] })
	}
	var out []centralstore.Session
	queue := []centralstore.SessionID{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, rows[byID[id]])
		queue = append(queue, byParent[id]...)
	}
	return out, nil
}

// copySlice returns a non-nil shallow copy of s. When s is empty the
// result is an allocated empty slice so JSON marshaling produces []
// instead of null — the nil-vs-empty bug class (ADR 0026 FD-3).
// renderStoreDirect renders the full wire Frames from SQLite at request
// time. This is the store-direct read path (ADR 0026 §2a): one SQLite read
// txn + runtime overlay + wire conversion. The result is identical to what
// the next composed fanout frame would produce, but fresher.
func renderStoreDirect(ctx context.Context, boot *Bootstrap, converter *wire.Converter, peerAdapter *centralPeerAdapter) (wire.Frames, error) {
	batch, err := central.RenderAll(
		ctx,
		boot.Store,
		boot.Runtime,
		boot.Verdicts,
		central.PeerSourceFunc(peerAdapter.PeerWorld),
	)
	if err != nil {
		return wire.Frames{}, err
	}
	var peerRows []wire.Session
	if boot.cfg.PeerSessions != nil {
		peerRows = boot.cfg.PeerSessions.PeerSessions()
	}
	var out wire.Frames
	if batch.Sessions != nil {
		p := converter.Sessions(batch.Sessions, batch.Projects, peerRows)
		out.Sessions = &p
	}
	if batch.Projects != nil {
		w := converter.World(batch.Sessions, batch.Projects, peerRows)
		out.World = &w
	}
	return out, nil
}

// wireSessionFromStore builds a minimal wire.Session from a store row and
// the live registry. Used for store-direct lookups in scrollback/attach.
func wireSessionFromStore(row centralstore.Session, reg *sessioncoord.Registry) wire.Session {
	out := wire.Session{
		ID:        string(row.ID),
		CreatedAt: fmtMillis(row.CreatedAt),
		Adapter:   row.Adapter,
	}
	if row.DriveMode != centralstore.DriveModeTerminal {
		out.DriveMode = row.DriveMode
	}
	if row.TerminalCols != nil {
		out.TerminalCols = *row.TerminalCols
	}
	if row.TerminalRows != nil {
		out.TerminalRows = *row.TerminalRows
	}
	if runtime, live := registryRuntime(reg, row.ID); live {
		out.Alive = true
		out.SocketPath = runtime.Endpoint
	}
	return out
}

func copySlice[T any](s []T) []T {
	out := make([]T, len(s))
	copy(out, s)
	return out
}

func manualPeerResponse(peer centralstore.ManualPeer, outcome centralstore.PeerUpsertOutcome) map[string]any {
	if outcome == centralstore.PeerUnchanged {
		return map[string]any{"peer": peer, "already_connected": true}
	}
	return map[string]any{"peer": peer, "updated": outcome == centralstore.PeerUpdated}
}

// freshHealthCounts derives the FD-6 health session summary from the current
// sessions frame. The world frame (which embeds SessionCounts) is only
// recomposed on project/peer batches, so counts cached there go stale across
// liveness-only changes; read paths that promise request-time freshness
// (matching legacy's compose-at-emit behavior) recompute them from the
// sessions frame, which IS rebuilt on every liveness batch. Semantics mirror
// wire.deriveCounts: alive locals, alive peer rows, everything else dead.
func freshHealthCounts(frames wire.Frames) (central.SessionCounts, bool) {
	if frames.Sessions == nil {
		return central.SessionCounts{}, false
	}
	var counts central.SessionCounts
	for _, s := range frames.Sessions.Sessions {
		switch {
		case s.Alive && s.Peer == "":
			counts.LocalAlive++
		case s.Alive:
			counts.RemoteAlive++
		default:
			counts.Dead++
		}
	}
	return counts, true
}
