// Package snapshot composes the wire payloads for the protocol-2
// SSE stream defined by ADR 0001.
//
// The protocol has two snapshot kinds plus one bare event:
//
//   - snapshot.sessions  full list of owned sessions (replaces the
//     per-event session-upsert / session-remove
//     stream of protocol 1).
//   - snapshot.world     bundle of projects, peers, health, launchers
//     (replaces projects-update / peer-status).
//     Not sent to peer consumers.
//   - session-activity   bare {id} ping; lossy, not coalesced.
//
// Snapshots are composed lazily at emit time by a per-kind coalescer
// (see internal/coalesce); this package only knows how to assemble
// the payload from already-fetched inputs. Callers (the SSE handler
// in main.go) decide what to read from where and pass it in.
//
// This split keeps composition free of locks and HTTP plumbing, so
// the wire shape can be exercised in isolation.
package snapshot

import (
	"sort"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// SessionsPayload is the body of a snapshot.sessions SSE event.
//
// Sessions is a value-typed slice (not pointers) so the wire shape
// is stable regardless of how the caller reads them out of the store.
// ComposeSessions sorts it by ID, so two snapshots of identical state
// serialize to identical bytes — see ComposeSessions for why that
// determinism is load-bearing.
type SessionsPayload struct {
	Sessions []store.Session `json:"sessions"`
}

// WorldPayload is the body of a snapshot.world SSE event. It bundles
// every piece of cross-session state the frontend needs into one
// atomic emission, so the client can replace its `_rawWorld` signal
// in a single batch (no transient inconsistency between projects and
// peer status).
//
// The fields are typed `any` because their concrete types live in
// packages this one shouldn't depend on (projects.Item,
// peering.PeerInfo, peering.LauncherDef, the /v1/health map). The
// caller marshals them through; SSE clients see normal JSON.
//
// Health carries the same shape as GET /v1/health response data:
// hostname, tailscale, version, peers (offline-merged), session
// counts, runner_hash, etc. We re-emit it on each snapshot rather
// than splitting it across fields because it is intentionally an
// opaque diagnostic blob.
type WorldPayload struct {
	Projects        any `json:"projects"`
	Peers           any `json:"peers"`
	Health          any `json:"health"`
	Launchers       any `json:"launchers"`
	DefaultLauncher any `json:"default_launcher"`

	// PeerProjects enumerates each connected peer's projects so the
	// viewer can render references (projects owned by other hosts) in
	// the sidebar and Manage Projects modal without proxying a
	// separate request. Keyed by peer name. Empty for peer consumers
	// (they don't render references for hops they can't see anyway).
	//
	// Each entry carries the slug plus a launch_cwd hint derived from
	// the peer's first path rule, so the launch button on an empty
	// referenced folder has somewhere to land. Session counts and
	// last-active timestamps are derived client-side from stamped
	// sessions; nothing else needs to ride on this field.
	PeerProjects any `json:"peer_projects,omitempty"`

	// PeerDiscovered carries each connected peer's self-advertised
	// discovered list (sessions the peer owns but no project of its own
	// claims), keyed by peer name. Discovery is host-authoritative
	// (ADR 0002/0005): the owning host runs its own match rules over
	// its own sessions, so the viewer renders these rows verbatim
	// rather than recomputing peer discovery blind (which could offer a
	// project the peer already owns by a rule the viewer can't see).
	// Empty for peer consumers and for local peers (whose sessions flow
	// through the parent's local discovery).
	PeerDiscovered any `json:"peer_discovered,omitempty"`
}

// ComposeSessions builds a snapshot.sessions payload from the live
// store, keeping only sessions for which `owned` returns true.
//
// `owned` mirrors the per-subscriber filter the SSE handler applies
// for `?as=peer` consumers (own + Local-peer only). For browser
// consumers it should accept everything.
func ComposeSessions(all []store.Session, owned func(*store.Session) bool) SessionsPayload {
	out := make([]store.Session, 0, len(all))
	for i := range all {
		s := all[i]
		if owned == nil || owned(&s) {
			out = append(out, s)
		}
	}
	// Stable ordering: sort by ID. Without this the slice reflects
	// Go map iteration order, which is randomized per-iteration, so
	// two snapshots of identical state produce byte-different wire
	// payloads. That defeats any downstream byte-level deduping and
	// makes diagnostic logs ("why did this snapshot fire?") useless.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return SessionsPayload{Sessions: out}
}
