package wire

import (
	"sort"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// Converter holds the pure derivation policy injected at construction:
// per-adapter command titlers (production: adapters implementing
// adapter.CommandTitler, exactly what store.SetCommandTitlers received) and
// the resume-command resolver (production: discovery.ResolveResumeCommand's
// adapter lookup, as a function of (adapter, conversation ref)). Both may
// be nil; IsLocalPeer identifies Local-peer (devcontainer) names for stamp
// ownership and the ?as=peer filter.
type Converter struct {
	// Titlers maps adapter name → command-title derivation.
	Titlers map[string]func([]string) string
	// ResumeCommand derives the resume form of a dead session's command
	// from its adapter + opaque conversation ref. A nil func or a nil
	// return keeps the recorded launch command (production
	// subs.OnExit/Scan parity).
	ResumeCommand func(adapterName, conversationRef string) []string
	// IsLocalPeer reports whether a peer name is a Local peer (an
	// extension of this host whose project stamps the parent owns).
	IsLocalPeer func(name string) bool
}

// resolveTitle reproduces store.resolveTitle's precedence exactly:
// adapter title > shell title > CommandTitler(command) > adapter name.
// (The store's final sess.Title fallback is unreachable post-cutover:
// centralstore requires a non-empty adapter.)
func (c *Converter) resolveTitle(s centralstore.Session) string {
	if s.AdapterTitle != "" {
		return s.AdapterTitle
	}
	if s.ShellTitle != "" {
		return s.ShellTitle
	}
	if fn := c.Titlers[s.Adapter]; fn != nil && len(s.Command) > 0 {
		return fn(s.Command)
	}
	return s.Adapter
}

// fmtMillis converts a durable Unix-ms stamp to the wire's RFC3339 form.
// time.RFC3339 has no fractional-second component, so this matches the
// production nowRFC3339 second precision (FD-4).
func fmtMillis(v centralstore.UnixMillis) string {
	return time.UnixMilli(int64(v)).UTC().Format(time.RFC3339)
}

func fmtMillisPtr(v *centralstore.UnixMillis) string {
	if v == nil {
		return ""
	}
	return fmtMillis(*v)
}

// session converts one composed local row: durable projection + runtime
// overlay + derived title/status/resumable/rewritten command. Placement
// stamps (project_slug/project_index) are applied by the payload-level
// flatten, not here.
func (c *Converter) session(row central.SessionRow) Session {
	v := row.Session
	out := Session{
		ID:              string(v.ID),
		CreatedAt:       fmtMillis(v.CreatedAt),
		Command:         v.Command,
		Cwd:             v.CWD,
		Adapter:         v.Adapter,
		WorkspaceRoot:   v.WorkspaceRoot,
		Remotes:         v.Remotes,
		Alive:           row.Alive,
		ExitCode:        v.ExitCode,
		StartedAt:       fmtMillisPtr(v.StartedAt),
		ExitedAt:        fmtMillisPtr(v.ExitedAt),
		Title:           c.resolveTitle(v),
		Subtitle:        v.Subtitle,
		Unread:          v.Unread,
		Slug:            v.Slug,
		ConversationRef: v.ConversationRef,
		LastOutputAt:    fmtMillisPtr(v.LastActivityAt),
	}
	// "status": null until the runner reports one — the durable
	// status_reported fact carries production's Status-pointer nil-ness
	// (gmux wait derives died-vs-idle from exactly this, ADR 0023).
	if v.StatusReported {
		out.Status = &Status{Working: v.Working, Error: v.Error}
	}
	if v.LaunchParentID != nil {
		out.ParentSessionID = string(*v.LaunchParentID)
	}
	if v.TerminalCols != nil {
		out.TerminalCols = *v.TerminalCols
	}
	if v.TerminalRows != nil {
		out.TerminalRows = *v.TerminalRows
	}
	if row.Alive && row.Runtime != nil {
		out.Pid = row.Runtime.PID
		out.SocketPath = row.Runtime.Endpoint
		out.RunnerVersion = row.Runtime.RunnerVersion
		out.BinaryHash = row.Runtime.BinaryHash
	}
	if !row.Alive {
		// Resume-command rewriting is presentation/spawn policy, never
		// durable state: the row keeps the launch command, the wire shows
		// the resume form (design §3.1). RunnerSpawner applies the same
		// pure function at spawn.
		if c.ResumeCommand != nil && v.ConversationRef != "" {
			if cmd := c.ResumeCommand(v.Adapter, v.ConversationRef); cmd != nil {
				out.Command = cmd
			}
		}
		// Narrowing: the composer's Resumable already folds in the verdict
		// (dead ∧ durable command ∧ verdict ≠ Gone). Recompute the command
		// term against the rewritten command so a row whose durable command
		// is empty but whose conversation still resolves to a resume form
		// becomes resumable — production parity, where Resumable derives
		// from the post-rewrite command.
		gone := len(v.Command) > 0 && !row.Resumable
		out.Resumable = len(out.Command) > 0 && !gone
	}
	if row.Session.DismissedAt != nil {
		// Defensive: ReadSnapshot filters dismissed rows (FD-2); a row that
		// slips through must still never reach the wire.
		out.Resumable = false
	}
	return out
}

// Sessions converts a composed sessions payload plus the peer-session
// overlay into the snapshot.sessions wire payload. world may be nil (no
// projects payload composed yet); Local-peer placement stamps and FD-1
// indices then cover local placements only.
func (c *Converter) Sessions(local *central.SessionsPayload, world *central.ProjectsPayload, peerRows []Session) SessionsPayload {
	var localRows []Session
	var views []central.SessionRow
	if local != nil {
		views = local.Sessions
		localRows = make([]Session, len(views))
		for i, row := range views {
			localRows[i] = c.session(row)
		}
	}

	// Copy peer rows: network-peer rows pass through verbatim (origin
	// stamps, ADR 0025); Local-peer rows get parent-owned stamps from the
	// durable placement join (or cleared stamps when unplaced).
	peers := make([]Session, len(peerRows))
	copy(peers, peerRows)
	placementByPeerKey := map[[2]string]central.LocalPeerPlacementRow{}
	if world != nil {
		for _, p := range world.LocalPeerPlacements {
			placementByPeerKey[[2]string{string(p.PeerKey), p.SessionID}] = p
		}
	}
	peerIdxByKey := map[[2]string]int{}
	for i := range peers {
		if peers[i].Peer == "" || c.IsLocalPeer == nil || !c.IsLocalPeer(peers[i].Peer) {
			continue
		}
		orig, _ := peering.ParseID(peers[i].ID)
		key := [2]string{peers[i].Peer, orig}
		peerIdxByKey[key] = i
		if p, ok := placementByPeerKey[key]; ok {
			peers[i].ProjectSlug = p.ProjectSlug
			peers[i].ProjectIndex = 0 // stamped by the flatten below
		} else {
			peers[i].ProjectSlug = ""
			peers[i].ProjectIndex = 0
		}
	}

	// FD-1 flatten: durable scoped ordering → legacy flat per-project
	// project_index.
	for _, nodes := range flattenAll(views, worldPlacements(world)) {
		for idx, n := range nodes {
			if n.localID != "" {
				for i := range localRows {
					if localRows[i].ID == n.localID {
						localRows[i].ProjectSlug = n.slug
						localRows[i].ProjectIndex = idx
					}
				}
				continue
			}
			if i, ok := peerIdxByKey[[2]string{n.peer, n.peerSession}]; ok {
				peers[i].ProjectSlug = n.slug
				peers[i].ProjectIndex = idx
			}
		}
	}

	merged := make([]Session, 0, len(localRows)+len(peers))
	merged = append(merged, localRows...)
	merged = append(merged, peers...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return SessionsPayload{Sessions: merged}
}

// World converts a composed projects payload (plus the session inputs the
// FD-5 sessions[] rebuild and FD-6 counts need) into the snapshot.world
// wire payload.
func (c *Converter) World(local *central.SessionsPayload, world *central.ProjectsPayload, peerRows []Session) WorldPayload {
	if world == nil {
		return WorldPayload{
			Projects:  []ProjectItem{},
			Peers:     []peering.PeerInfo{},
			Launchers: []peering.LauncherDef{},
		}
	}
	var views []central.SessionRow
	if local != nil {
		views = local.Sessions
	}
	flat := flattenAll(views, worldPlacements(world))

	items := make([]ProjectItem, 0, len(world.Projects))
	for _, e := range world.Projects {
		item := ProjectItem{Slug: e.Slug}
		if e.Kind == centralstore.ProjectEntryReference {
			item.Peer = string(e.PeerKey)
			item.NodeID = e.NodeID
		} else {
			for _, r := range e.Rules {
				item.Match = append(item.Match, MatchRule{Path: r.Path, Remote: r.Remote, Exact: r.Exact})
			}
			for _, n := range flat[e.Slug] {
				item.Sessions = append(item.Sessions, n.wireKey)
			}
		}
		items = append(items, item)
	}

	out := WorldPayload{
		Projects:        items,
		Peers:           ensurePeerSlice(world.Peers),
		Launchers:       ensureLauncherSlice(world.Launchers),
		DefaultLauncher: world.DefaultLauncher,
		PeerProjects:    world.PeerProjects,
		PeerDiscovered:  world.PeerDiscovered,
	}
	if world.Health != nil {
		h := *world.Health // copy: never mutate the PeerSource's blob
		h.Sessions = deriveCounts(views, peerRows)
		out.Health = &h
	}
	return out
}

// deriveCounts computes the FD-6 health session summary from durable rows +
// runtime liveness + peer projections. Dismissed rows never reach the
// composed payload, so they no longer count as dead (accepted diff FD-6).
func deriveCounts(views []central.SessionRow, peerRows []Session) central.SessionCounts {
	var counts central.SessionCounts
	for _, v := range views {
		if v.Alive {
			counts.LocalAlive++
		} else {
			counts.Dead++
		}
	}
	for _, s := range peerRows {
		if s.Alive {
			counts.RemoteAlive++
		} else {
			counts.Dead++
		}
	}
	return counts
}

// ── FD-1 flatten ──

// flatNode is one FD-1 participant: a placed local session or a connected
// placed Local-peer session.
type flatNode struct {
	slug        string
	localID     string // non-empty for local sessions
	peer        string // Local-peer arm
	peerSession string
	wireKey     string // sessions[] key: local ID or namespaced "id@peer"
	nodeKey     string // sibling-scope key ("l:<id>" / "p:<peer>:<sess>", escaped)
	scope       string // durable sibling scope
	pos         int    // durable dense position within the scope
}

func worldPlacements(world *central.ProjectsPayload) []central.LocalPeerPlacementRow {
	if world == nil {
		return nil
	}
	return world.LocalPeerPlacements
}

// flattenAll computes, per project slug, the FD-1 display order: roots in
// root-scope order, each visible child block inlined directly after its
// parent in child-scope order (recursively), renumbered 0..n-1
// project-wide. The wire keeps the legacy flat project_index; the durable
// state keeps the dense per-scope ordering.
func flattenAll(views []central.SessionRow, placements []central.LocalPeerPlacementRow) map[string][]flatNode {
	byProject := map[string][]flatNode{}
	for _, row := range views {
		p := row.Placement
		if p == nil {
			continue
		}
		byProject[p.ProjectSlug] = append(byProject[p.ProjectSlug], flatNode{
			slug: p.ProjectSlug, localID: string(row.ID), wireKey: string(row.ID),
			nodeKey: "l:" + string(row.ID), scope: p.SiblingScope, pos: p.Position,
		})
	}
	for _, p := range placements {
		byProject[p.ProjectSlug] = append(byProject[p.ProjectSlug], flatNode{
			slug: p.ProjectSlug, peer: string(p.PeerKey), peerSession: p.SessionID,
			wireKey: peering.NamespaceID(p.SessionID, string(p.PeerKey)),
			nodeKey: "p:" + escapeScope(string(p.PeerKey)) + ":" + escapeScope(p.SessionID),
			scope:   p.SiblingScope, pos: p.Position,
		})
	}
	out := map[string][]flatNode{}
	for slug, nodes := range byProject {
		out[slug] = flattenProject(nodes)
	}
	return out
}

func flattenProject(nodes []flatNode) []flatNode {
	byScope := map[string][]flatNode{}
	for _, n := range nodes {
		byScope[n.scope] = append(byScope[n.scope], n)
	}
	for _, s := range byScope {
		sort.Slice(s, func(i, j int) bool { return s[i].pos < s[j].pos })
	}
	out := make([]flatNode, 0, len(nodes))
	var walk func(n flatNode)
	walk = func(n flatNode) {
		out = append(out, n)
		for _, child := range byScope["c:"+n.nodeKey] {
			walk(child)
		}
	}
	for _, root := range byScope["r"] {
		walk(root)
	}
	// Defensive: placements whose scope references a parent outside this
	// project's participant set (skew between payload kinds) still get an
	// index, appended after the reachable tree in scope/pos order.
	if len(out) < len(nodes) {
		seen := map[string]bool{}
		for _, n := range out {
			seen[n.nodeKey] = true
		}
		var rest []flatNode
		for _, n := range nodes {
			if !seen[n.nodeKey] {
				rest = append(rest, n)
			}
		}
		sort.Slice(rest, func(i, j int) bool {
			if rest[i].scope != rest[j].scope {
				return rest[i].scope < rest[j].scope
			}
			return rest[i].pos < rest[j].pos
		})
		out = append(out, rest...)
	}
	return out
}

// escapeScope mirrors centralstore's sibling-scope key escaping (its
// unexported escape): ':' and '%' are escaped so peer keys and session IDs
// cannot forge scope separators. The FD-1 golden round-trip tests pin this
// against the real store's scopes.
func escapeScope(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), ":", "%3A")
}

func unescapeScope(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%3A", ":"), "%25", "%")
}

// ensurePeerSlice returns a non-nil empty slice when the input is nil,
// so JSON marshaling produces [] instead of null.
func ensurePeerSlice(s []peering.PeerInfo) []peering.PeerInfo {
	if s == nil {
		return []peering.PeerInfo{}
	}
	return s
}

// ensureLauncherSlice returns a non-nil empty slice when the input is nil.
func ensureLauncherSlice(s []peering.LauncherDef) []peering.LauncherDef {
	if s == nil {
		return []peering.LauncherDef{}
	}
	return s
}
