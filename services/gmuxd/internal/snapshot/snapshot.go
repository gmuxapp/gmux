// Package snapshot composes the wire payloads for the protocol-2
// SSE stream defined by ADR 0001.
//
// The protocol has two snapshot kinds plus one bare event:
//
//   - snapshot.sessions  full list of owned sessions (replaces the
//                        per-event session-upsert / session-remove
//                        stream of protocol 1).
//   - snapshot.world     bundle of projects, peers, health, launchers
//                        (replaces projects-update / peer-status).
//                        Not sent to peer consumers.
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

import "github.com/gmuxapp/gmux/services/gmuxd/internal/store"

// SessionsPayload is the body of a snapshot.sessions SSE event.
//
// Sessions is a value-typed slice (not pointers) so the wire shape
// is stable regardless of how the caller reads them out of the store.
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
	return SessionsPayload{Sessions: out}
}
