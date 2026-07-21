package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
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
	ActivityID     string
	ProjectsUpdate bool
}

type sseFanout struct {
	mu       sync.Mutex
	sessions *wire.SessionsPayload
	world    *wire.WorldPayload
	subs     map[chan fanoutMessage]struct{}
}

func newSSEFanout() *sseFanout { return &sseFanout{subs: make(map[chan fanoutMessage]struct{})} }

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

func (f *sseFanout) Subscribe() (wire.Frames, <-chan fanoutMessage, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan fanoutMessage, 32)
	f.subs[ch] = struct{}{}
	initial := f.currentLocked()
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
	msg := fanoutMessage{Frames: frames, ProjectsUpdate: frames.World != nil}
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
		status = &compatStatus{Working: row.Working, Error: row.Error}
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
		status = &compatStatus{Working: s.Status.Working, Error: s.Status.Error}
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

func sessionTreeRows(ctx context.Context, st *centralstore.Store, root centralstore.SessionID) ([]centralstore.Session, error) {
	rows, err := st.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	byParent := make(map[centralstore.SessionID][]centralstore.Session)
	present := false
	for _, row := range rows {
		if row.ID == root {
			present = true
		}
		if row.LaunchParentID != nil {
			byParent[*row.LaunchParentID] = append(byParent[*row.LaunchParentID], row)
		}
	}
	if !present {
		return nil, fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, root)
	}
	for _, kids := range byParent {
		sort.Slice(kids, func(i, j int) bool { return kids[i].ID < kids[j].ID })
	}
	var out []centralstore.Session
	queue := []centralstore.SessionID{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, row := range rows {
			if row.ID == id {
				out = append(out, row)
				break
			}
		}
		for _, child := range byParent[id] {
			queue = append(queue, child.ID)
		}
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
