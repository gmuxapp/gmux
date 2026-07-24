// Package equivalence is the S2 semantic-equivalence gate (design-cutover
// §8): one seed model (World) rendered through BOTH snapshot paths —
//
//   - the production composer: internal/store + internal/projects +
//     internal/snapshot (imported read-only; deleted at the S5 switch);
//   - the new stack: a real internal/centralstore DB + registry/verdict/
//     peer fakes + internal/snapshot/central + internal/snapshot/wire —
//
// and compared as structural JSON with the FD-* fields excluded, plus one
// targeted assertion per accepted diff FD-1..FD-6. This suite is the S5
// switch gate: extend World (and the per-FD tests) rather than writing new
// ad-hoc comparisons.
package equivalence

import (
	"context"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// FixtureSession seeds one session in both renderers.
type FixtureSession struct {
	ID              string
	Adapter         string
	Command         []string
	Cwd             string
	WorkspaceRoot   string
	Remotes         map[string]string
	Slug            string
	ShellTitle      string
	AdapterTitle    string
	Subtitle        string
	ConversationRef string

	Alive         bool
	Pid           int
	SocketPath    string
	RunnerVersion string
	BinaryHash    string

	ExitCode                   *int
	Created, Started, Exited   time.Time
	LastActivity               time.Time
	Working, Error, Unread     bool
	TerminalCols, TerminalRows uint16

	Parent string // launch-parent session ID ("" = root)
	// Promoted marks a launched child promoted back to a sidebar root
	// (FD-1: promoted ⇒ root scope regardless of parent).
	Promoted bool
	// StatusNeverReported seeds the production Status == nil /
	// status_reported = 0 state: the runner never reported a status
	// (must not be combined with Working/Error).
	StatusNeverReported bool

	// Peer marks a peer-owned projection: "" = local. Local peers (per
	// World.LocalPeers) ride the Local-peer arm; other names are
	// network-peer mirrors passed through verbatim. Peer session IDs are
	// given namespaced ("orig@peer").
	Peer string
	// PeerProjectSlug/Index are the origin stamps riding a NETWORK peer
	// projection.
	PeerProjectSlug  string
	PeerProjectIndex int

	// Dismissed exists only in the new model (hidden-not-forgotten);
	// production simply never sees the session (FD-2).
	Dismissed bool
}

// FixtureProject seeds one owned project. Sessions lists the sidebar order
// as wire keys (local IDs / namespaced Local-peer IDs), in FD-1 flatten
// order (parents directly followed by their children) so the two paths
// agree wherever equivalence is asserted.
type FixtureProject struct {
	Slug     string
	Rules    []projects.MatchRule
	Sessions []string
}

// FixtureReference seeds one peer-project reference entry. NodeID is the
// ADR 0017 liveness anchor (empty = created against a pre-ADR-0007 daemon).
type FixtureReference struct {
	Slug, Peer, NodeID string
}

// World is the seed model.
type World struct {
	Sessions   []FixtureSession
	Projects   []FixtureProject
	References []FixtureReference
	LocalPeers []string

	Peers           []peering.PeerInfo
	Launchers       []peering.LauncherDef
	DefaultLauncher string
	PeerProjects    map[string][]peering.SpokeProject
	PeerDiscovered  map[string][]peering.SpokeDiscovered

	Hostname, Version, NodeID, Listen, RunnerHash string
}

func (w *World) isLocalPeer(name string) bool {
	for _, p := range w.LocalPeers {
		if p == name {
			return true
		}
	}
	return false
}

// Titlers is the shared command-titler set injected into both paths.
func Titlers() map[string]func([]string) string {
	return map[string]func([]string) string{
		"codex": func(cmd []string) string { return "run:" + cmd[0] },
	}
}

// ResumeCommand is the shared pure resume-command function (production
// applies it at exit time and persists the result in the in-memory store;
// the new stack applies it at wire conversion — same function, same
// output).
func ResumeCommand(adapterName, conversationRef string) []string {
	if conversationRef == "" {
		return nil
	}
	return []string{adapterName, "resume", conversationRef}
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func millisPtr(t time.Time) *centralstore.UnixMillis {
	if t.IsZero() {
		return nil
	}
	m := centralstore.UnixMillis(t.UnixMilli())
	return &m
}

// RenderProduction renders the World through the production composition
// path: store.Store (Upsert/UpsertRemote), projects.State stamping
// (AssignmentsByKey, mirroring cmd/gmuxd reconcileProjectStamps), and
// snapshot.ComposeSessions / snapshot.WorldPayload.
func RenderProduction(w *World) (snapshot.SessionsPayload, snapshot.WorldPayload) {
	st := store.New()
	st.SetCommandTitlers(Titlers())

	state := &projects.State{Version: 4}
	for _, p := range w.Projects {
		state.Items = append(state.Items, projects.Item{Slug: p.Slug, Match: p.Rules, Sessions: p.Sessions})
	}
	for _, r := range w.References {
		state.Items = append(state.Items, projects.Item{Slug: r.Slug, Peer: r.Peer, NodeID: r.NodeID})
	}
	assignments := state.AssignmentsByKey()

	for _, f := range w.Sessions {
		if f.Dismissed {
			continue // production has no dismissed state: the row is gone
		}
		sess := store.Session{
			ID: f.ID, Peer: f.Peer, CreatedAt: rfc3339(f.Created), Command: f.Command,
			Cwd: f.Cwd, Adapter: f.Adapter, WorkspaceRoot: f.WorkspaceRoot, Remotes: f.Remotes,
			ParentSessionID: f.Parent, Alive: f.Alive, ExitCode: f.ExitCode,
			StartedAt: rfc3339(f.Started), ExitedAt: rfc3339(f.Exited),
			Subtitle: f.Subtitle, Status: fixtureStatus(f),
			Unread: f.Unread, TerminalCols: f.TerminalCols, TerminalRows: f.TerminalRows,
			Slug: f.Slug, ConversationRef: f.ConversationRef,
			LastOutputAt: rfc3339(f.LastActivity),
			ShellTitle:   f.ShellTitle, AdapterTitle: f.AdapterTitle,
		}
		if f.Alive {
			sess.Pid = f.Pid
			sess.SocketPath = f.SocketPath
			sess.RunnerVersion = f.RunnerVersion
			sess.BinaryHash = f.BinaryHash
		}
		if f.Peer != "" {
			// Peer projection: fully resolved on the origin. Title rides
			// the wire; UpsertRemote never re-derives it. Project stamps
			// arrive as-received for every peer kind — Local-peer residue
			// included — and reconcileProjectStamps below re-stamps or
			// clears the Local-peer ones (parent-owned assignment).
			sess.Title = deriveFixtureTitle(f)
			sess.Resumable = !f.Alive && len(f.Command) > 0
			sess.ProjectSlug = f.PeerProjectSlug
			sess.ProjectIndex = f.PeerProjectIndex
			st.UpsertRemote(sess)
			continue
		}
		if !f.Alive {
			// subs.OnExit parity: the resume form replaces the launch
			// command at death.
			if cmd := ResumeCommand(f.Adapter, f.ConversationRef); cmd != nil {
				sess.Command = cmd
			}
		}
		st.Upsert(sess)
	}

	// reconcileProjectStamps parity (cmd/gmuxd/main.go): local and
	// Local-peer sessions stamp from the parent's assignments; network
	// peers keep origin stamps.
	st.Reconcile(func(s store.Session) (string, int) {
		if s.Peer != "" && !w.isLocalPeer(s.Peer) {
			return s.ProjectSlug, s.ProjectIndex
		}
		a := assignments[s.ID]
		return a.Slug, a.Index
	})

	world := snapshot.WorldPayload{
		Projects:        state.Items,
		Peers:           w.Peers,
		Health:          productionHealth(w, st),
		Launchers:       w.Launchers,
		DefaultLauncher: w.DefaultLauncher,
		PeerProjects:    w.PeerProjects,
		PeerDiscovered:  w.PeerDiscovered,
	}
	return snapshot.ComposeSessions(st.List(), nil), world
}

// RenderProductionOwnedSessions renders the production ?as=peer sessions
// payload: ComposeSessions with the owned filter (own + Local-peer only),
// mirroring the SSE handler's isOwned closure in cmd/gmuxd/main.go.
func RenderProductionOwnedSessions(w *World) snapshot.SessionsPayload {
	full, _ := RenderProduction(w)
	return snapshot.ComposeSessions(full.Sessions, func(s *store.Session) bool {
		return s.Peer == "" || w.isLocalPeer(s.Peer)
	})
}

// productionHealth mirrors cmd/gmuxd composeHealth (minus tailscale/update
// fields the fixture leaves unset, and minus auth_token which never rides
// snapshot.world).
func productionHealth(w *World, st *store.Store) map[string]any {
	var localAlive, remoteAlive, dead int
	for _, s := range st.List() {
		switch {
		case !s.Alive:
			dead++
		case s.Peer == "":
			localAlive++
		default:
			remoteAlive++
		}
	}
	data := map[string]any{
		"service": "gmuxd", "version": w.Version, "node_id": w.NodeID, "status": "ready",
		"hostname": w.Hostname, "listen": w.Listen,
		"sessions":    map[string]int{"local_alive": localAlive, "remote_alive": remoteAlive, "dead": dead},
		"runner_hash": w.RunnerHash, "default_launcher": w.DefaultLauncher, "launchers": w.Launchers,
	}
	if len(w.Peers) > 0 {
		data["peers"] = w.Peers
	}
	return data
}

func fixtureStatus(f FixtureSession) *store.Status {
	if f.StatusNeverReported {
		return nil
	}
	return &store.Status{Working: f.Working, Error: f.Error}
}

func deriveFixtureTitle(f FixtureSession) string {
	if f.AdapterTitle != "" {
		return f.AdapterTitle
	}
	if f.ShellTitle != "" {
		return f.ShellTitle
	}
	if fn := Titlers()[f.Adapter]; fn != nil && len(f.Command) > 0 {
		return fn(f.Command)
	}
	return f.Adapter
}

// RenderCentral renders the World through the new stack: a real
// centralstore DB, registry/verdict/peer fakes, the central composer, and
// the wire conversion Cache. Returns the wire frames plus the Cache for
// follow-up assertions.
func RenderCentral(t *testing.T, w *World) (wire.Frames, *wire.Cache) {
	t.Helper()
	ctx := context.Background()
	db, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Catalog.
	var specs []centralstore.ProjectEntrySpec
	for _, p := range w.Projects {
		var rules []centralstore.MatchRule
		for _, r := range p.Rules {
			rules = append(rules, centralstore.MatchRule{Path: r.Path, Remote: r.Remote, Exact: r.Exact})
		}
		specs = append(specs, centralstore.ProjectEntrySpec{Owned: &centralstore.OwnedProjectSpec{Slug: p.Slug, Rules: rules}})
	}
	for _, r := range w.References {
		specs = append(specs, centralstore.ProjectEntrySpec{Reference: &centralstore.ProjectReference{PeerKey: centralstore.PeerKey(r.Peer), Slug: r.Slug, NodeID: r.NodeID}})
	}
	catalog, _, err := db.ReplaceProjectCatalog(ctx, specs, 0)
	if err != nil {
		t.Fatal(err)
	}
	projectBySlug := map[string]centralstore.ProjectEntryID{}
	for _, e := range catalog {
		if e.Kind == centralstore.ProjectEntryOwned {
			projectBySlug[e.Slug] = e.ID
		}
	}
	placementProject := map[string]string{} // wire key → project slug
	for _, p := range w.Projects {
		for _, key := range p.Sessions {
			placementProject[key] = p.Slug
		}
	}

	// Durable rows + placements, in fixture sidebar order (placement
	// append order is the durable ordering).
	runtime := map[centralstore.SessionID]central.RuntimeFacts{}
	lpConnected := map[central.LocalPeerSessionKey]struct{}{}
	var peerRows []wire.Session
	for _, f := range w.Sessions {
		if f.Peer != "" {
			row := peerFixtureRow(w, f)
			peerRows = append(peerRows, row)
			if w.isLocalPeer(f.Peer) {
				orig, _ := peering.ParseID(f.ID)
				lpConnected[central.LocalPeerSessionKey{PeerKey: centralstore.PeerKey(f.Peer), SessionID: orig}] = struct{}{}
				if slug := placementProject[f.ID]; slug != "" {
					if _, err := db.UpsertLocalPeerPlacement(ctx, centralstore.LocalPeerSubject{PeerKey: centralstore.PeerKey(f.Peer), SessionID: orig}, projectBySlug[slug]); err != nil {
						t.Fatal(err)
					}
				}
			}
			continue
		}
		ns := centralstore.NewSession{
			ID: centralstore.SessionID(f.ID), Adapter: f.Adapter, ConversationRef: f.ConversationRef,
			Command: f.Command, CWD: f.Cwd, WorkspaceRoot: f.WorkspaceRoot, Remotes: f.Remotes,
			Slug: f.Slug, ShellTitle: f.ShellTitle, AdapterTitle: f.AdapterTitle, Subtitle: f.Subtitle,
			Working: f.Working, Unread: f.Unread, Error: f.Error,
			StatusReported: !f.StatusNeverReported,
			CreatedAt:      centralstore.UnixMillis(f.Created.UnixMilli()),
			StartedAt:      millisPtr(f.Started), ExitedAt: millisPtr(f.Exited),
			LastActivityAt: millisPtr(f.LastActivity), ExitCode: f.ExitCode,
		}
		if f.TerminalCols != 0 {
			c, r := f.TerminalCols, f.TerminalRows
			ns.TerminalCols, ns.TerminalRows = &c, &r
		}
		if f.Parent != "" {
			p := centralstore.SessionID(f.Parent)
			ns.LaunchParentID = &p
		}
		if _, _, err := db.InsertSession(ctx, ns); err != nil {
			t.Fatal(err)
		}
		if slug := placementProject[f.ID]; slug != "" {
			if _, err := db.PlaceLocalSession(ctx, centralstore.SessionID(f.ID), projectBySlug[slug]); err != nil {
				t.Fatal(err)
			}
		}
		if f.Promoted {
			if _, err := db.SetPromotion(ctx, centralstore.SessionID(f.ID), true, nil); err != nil {
				t.Fatal(err)
			}
		}
		if f.Dismissed {
			if _, _, err := db.DismissSessionTree(ctx, centralstore.SessionID(f.ID), centralstore.UnixMillis(time.Now().UnixMilli())); err != nil {
				t.Fatal(err)
			}
		}
		if f.Alive {
			runtime[centralstore.SessionID(f.ID)] = central.RuntimeFacts{
				PID: f.Pid, Endpoint: f.SocketPath, RunnerVersion: f.RunnerVersion, BinaryHash: f.BinaryHash,
			}
		}
	}

	peerWorld := central.PeerWorld{
		Peers: w.Peers,
		Health: &central.HealthInfo{
			Service: "gmuxd", Version: w.Version, NodeID: w.NodeID, Status: "ready",
			Hostname: w.Hostname, Listen: w.Listen, RunnerHash: w.RunnerHash,
			DefaultLauncher: w.DefaultLauncher, Launchers: w.Launchers, Peers: w.Peers,
		},
		Launchers: w.Launchers, DefaultLauncher: w.DefaultLauncher,
		PeerProjects: w.PeerProjects, PeerDiscovered: w.PeerDiscovered,
		LocalPeerSessions: lpConnected,
	}

	// One composition pass through the real composer.
	batchCh := make(chan central.Batch, 1)
	comp := central.New(db,
		central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts { return runtime }),
		central.SinkFunc(func(_ context.Context, b central.Batch) { batchCh <- b }),
		central.WithPeerSource(central.PeerSourceFunc(func() central.PeerWorld { return peerWorld })),
	)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); comp.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	comp.MarkDirty(true, true)
	var batch central.Batch
	select {
	case batch = <-batchCh:
	case <-time.After(5 * time.Second):
		t.Fatal("composer emitted nothing")
	}

	conv := &wire.Converter{Titlers: Titlers(), ResumeCommand: ResumeCommand, IsLocalPeer: w.isLocalPeer}
	rows := peerRows
	cache := wire.NewCache(conv, wire.PeerSessionSourceFunc(func() []wire.Session { return rows }))
	return cache.Apply(batch), cache
}

// peerFixtureRow builds the peer manager's projection for a peer-owned
// fixture session: exactly the row the origin's own wire emitted (which is
// what production UpsertRemote stored verbatim).
func peerFixtureRow(w *World, f FixtureSession) wire.Session {
	row := wire.Session{
		ID: f.ID, Peer: f.Peer, CreatedAt: rfc3339(f.Created), Command: f.Command,
		Cwd: f.Cwd, Adapter: f.Adapter, WorkspaceRoot: f.WorkspaceRoot, Remotes: f.Remotes,
		ParentSessionID: f.Parent, Alive: f.Alive, ExitCode: f.ExitCode,
		StartedAt: rfc3339(f.Started), ExitedAt: rfc3339(f.Exited),
		Title: deriveFixtureTitle(f), Subtitle: f.Subtitle,
		Status: &wire.Status{Working: f.Working, Error: f.Error}, Unread: f.Unread,
		Resumable: !f.Alive && len(f.Command) > 0,
		Slug:      f.Slug, ConversationRef: f.ConversationRef,
		TerminalCols: f.TerminalCols, TerminalRows: f.TerminalRows,
		LastOutputAt: rfc3339(f.LastActivity),
	}
	if f.Alive {
		row.Pid = f.Pid
		row.SocketPath = f.SocketPath
		row.RunnerVersion = f.RunnerVersion
		row.BinaryHash = f.BinaryHash
	}
	// Stamps ride as-received for every peer kind: origin-authoritative
	// for network peers, stale residue for Local-peer rows — the wire
	// layer must re-stamp placed Local-peer rows and CLEAR unplaced ones
	// (tests review M-3 arm).
	row.ProjectSlug = f.PeerProjectSlug
	row.ProjectIndex = f.PeerProjectIndex
	return row
}

// DefaultWorld is the canonical fixture: live+dead+dismissed+peer+
// Local-peer sessions, launch hierarchy, catalog with a reference,
// empty-title edge cases, and sub-second timestamps (design §8 coverage
// list).
func DefaultWorld() *World {
	base := time.Date(2026, 7, 16, 10, 30, 0, 750_000_000, time.UTC) // sub-second component
	exit := 1
	return &World{
		LocalPeers: []string{"box"},
		Sessions: []FixtureSession{
			{
				ID: "sess-live", Adapter: "codex", Command: []string{"codex", "--full-auto"},
				Cwd: "/work/app", WorkspaceRoot: "/work/app", Remotes: map[string]string{"origin": "git@x:app"},
				AdapterTitle: "Fix auth flow", Subtitle: "3 files", Slug: "fix-auth",
				ConversationRef: "/conv/live.jsonl", Alive: true, Pid: 101, SocketPath: "/tmp/sess-live.sock",
				RunnerVersion: "2.0.0", BinaryHash: "hash-a", Working: true,
				Created: base, Started: base.Add(time.Second), LastActivity: base.Add(time.Minute),
				TerminalCols: 120, TerminalRows: 40,
			},
			{
				ID: "sess-child", Adapter: "shell", Command: []string{"nvim", "."},
				Cwd: "/work/app", ShellTitle: "nvim", Parent: "sess-live",
				Alive: true, Pid: 102, SocketPath: "/tmp/sess-child.sock",
				RunnerVersion: "2.0.0", BinaryHash: "hash-a",
				Created: base.Add(2 * time.Second),
			},
			{
				// Dead + resumable: conversation ref present, resume-command
				// rewrite applies; empty titles with a command and no titler.
				ID: "sess-dead", Adapter: "claude", Command: []string{"claude", "--continue"},
				Cwd: "/work/app", ConversationRef: "/conv/dead.jsonl",
				Alive: false, ExitCode: &exit, Unread: true, Error: true,
				Created: base.Add(-time.Hour), Started: base.Add(-time.Hour), Exited: base.Add(-30 * time.Minute),
				LastActivity: base.Add(-30 * time.Minute),
			},
			{
				// Empty-title edge: no titles, no command → adapter name.
				ID: "sess-bare", Adapter: "shell", Command: []string{},
				Cwd: "/work/lib", Alive: false,
				Created: base.Add(-2 * time.Hour),
			},
			{
				ID: "sess-hidden", Adapter: "shell", Command: []string{"bash"},
				Cwd: "/work/app", Alive: false, Dismissed: true,
				Created: base.Add(-3 * time.Hour),
			},
			{
				// Never-reported status: production Status == nil, wire
				// "status": null (fable M-1 — NOT an accepted diff). Also
				// breaks the alive/dead count symmetry (tests review H-1).
				ID: "sess-nostatus", Adapter: "shell", Command: []string{"bash"},
				Cwd: "/work/lib", Alive: true, Pid: 103, SocketPath: "/tmp/sess-nostatus.sock",
				StatusNeverReported: true,
				Created:             base.Add(5 * time.Second),
			},
			{
				// Promoted child (design §8 "incl. promoted children"): a
				// launched child promoted back to a sidebar root — FD-1
				// places it in the root scope, not under its parent.
				ID: "sess-promoted", Adapter: "shell", Command: []string{"htop"},
				Cwd: "/work/app", ShellTitle: "htop", Parent: "sess-live", Promoted: true,
				Alive: true, Pid: 104, SocketPath: "/tmp/sess-promoted.sock",
				Created: base.Add(6 * time.Second),
			},
			{
				// Local-peer (devcontainer) session, placed by the parent.
				// The projection carries stale residue stamps the placement
				// join must overwrite.
				ID: "cont-1@box", Peer: "box", Adapter: "shell", Command: []string{"bash"},
				Cwd: "/src", ShellTitle: "bash", Alive: true, Pid: 7, SocketPath: "/tmp/cont-1.sock",
				PeerProjectSlug: "stale-residue", PeerProjectIndex: 9,
				Created: base.Add(3 * time.Second),
			},
			{
				// UNPLACED Local-peer session whose projection carries stale
				// stamps — the wire must clear them (tests review M-3).
				ID: "cont-2@box", Peer: "box", Adapter: "shell", Command: []string{"bash"},
				Cwd: "/src/other", Alive: true, Pid: 8, SocketPath: "/tmp/cont-2.sock",
				PeerProjectSlug: "stale-residue", PeerProjectIndex: 4,
				Created: base.Add(7 * time.Second),
			},
			{
				// Network-peer mirror: origin stamps ride verbatim.
				ID: "remote-1@tower", Peer: "tower", Adapter: "codex", Command: []string{"codex"},
				Cwd: "/home/dev/x", AdapterTitle: "Remote task", Alive: true, Pid: 9,
				SocketPath: "/tmp/remote-1.sock", PeerProjectSlug: "tower-proj", PeerProjectIndex: 0,
				Created: base.Add(4 * time.Second), LastActivity: base.Add(5 * time.Second),
			},
		},
		Projects: []FixtureProject{{
			Slug:  "app",
			Rules: []projects.MatchRule{{Path: "/work/app"}},
			// FD-1 flatten order: parent directly followed by its child;
			// the promoted child sits at the root-scope tail, not under
			// its parent.
			Sessions: []string{"sess-live", "sess-child", "sess-dead", "sess-promoted", "cont-1@box"},
		}},
		References:      []FixtureReference{{Slug: "tower-proj", Peer: "tower", NodeID: "node-tower-1"}},
		Peers:           []peering.PeerInfo{{Name: "box", Status: "connected", Local: true, Source: "devcontainer"}, {Name: "tower", URL: "https://tower:443", Status: "connected", Version: "2.0.0"}},
		Launchers:       []peering.LauncherDef{{ID: "shell", Label: "Shell", Command: []string{"bash"}, Available: true}},
		DefaultLauncher: "shell",
		PeerProjects:    map[string][]peering.SpokeProject{"tower": {{Slug: "tower-proj", LaunchCwd: "/home/dev/x"}}},
		PeerDiscovered:  map[string][]peering.SpokeDiscovered{"tower": {{SuggestedSlug: "misc", Paths: []string{"/tmp"}, SessionCount: 1}}},
		Hostname:        "hub", Version: "2.0.0", NodeID: "node-1", Listen: "127.0.0.1:7337", RunnerHash: "hash-a",
	}
}
