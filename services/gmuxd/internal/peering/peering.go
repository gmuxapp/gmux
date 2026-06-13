// Package peering manages connections to remote gmuxd instances (spokes).
//
// Each gmuxd subscribes to its peers' GET /v1/events SSE streams and
// transforms remote sessions into the local store with namespaced IDs
// (originalID@peerName). Actions and WebSocket connections are routed
// back to the owning spoke by splitting on the last "@".
//
// # Hub-and-spoke session ownership
//
// Each node's SSE stream only includes sessions it owns: local sessions
// and sessions from Local peers (devcontainers connected via the Docker
// daemon). Network peer sessions are excluded from the outgoing stream.
// This prevents duplication when multiple peers aggregate each other.
// A Tailscale-connected container is independently reachable by every
// node on the tailnet, so it is not Local and its sessions are not
// forwarded. A Docker-only devcontainer is only reachable through its
// host, so it is Local and its sessions are forwarded.
//
// As a secondary defense, the receiver drops forwarded sessions whose
// origin peer is already a direct subscription (matched by name).
package peering

import (
	"net/http"
	"strings"
	"time"
)

// PeerOption configures a Peer at creation time.
type PeerOption func(*Peer)

// WithTransport sets a custom HTTP transport for all peer connections.
// Used for tailscale-discovered peers that route through tsnet. The
// transport is stored and applied when newPeer constructs the
// underlying apiclient.Client.
func WithTransport(rt http.RoundTripper) PeerOption {
	return func(p *Peer) {
		p.transport = rt
	}
}

// WithStreamIdleTimeout overrides the default SSE idle timeout for
// this peer. Intended for tests that need fast idle detection without
// waiting 60 seconds.
func WithStreamIdleTimeout(d time.Duration) PeerOption {
	return func(p *Peer) {
		p.streamIdleTimeout = d
	}
}

// Status describes the connection state of a peer.
type Status int

const (
	StatusDisconnected Status = iota
	StatusConnecting
	StatusConnected
)

func (s Status) String() string {
	switch s {
	case StatusConnecting:
		return "connecting"
	case StatusConnected:
		return "connected"
	default:
		return "disconnected"
	}
}

// LauncherDef describes a single launchable adapter on a gmuxd instance.
type LauncherDef struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Command     []string `json:"command"`
	Description string   `json:"description,omitempty"`
	Available   bool     `json:"available"`
}

// SpokeHealth is the subset of a spoke's /v1/health response that the
// hub caches and serves to the frontend. Parsed from the raw JSON on
// each connection.
type SpokeHealth struct {
	Version         string        `json:"version"`
	DefaultLauncher string        `json:"default_launcher"`
	Launchers       []LauncherDef `json:"launchers"`
	// NodeID is the spoke's stable opaque identity (ADR 0007). The
	// viewer anchors peer references on it so they survive the host
	// being renamed. Empty when talking to a pre-ADR-0007 daemon.
	NodeID string `json:"node_id"`
}

// SpokeProject is the minimal projection of a peer's project that
// the hub re-broadcasts in its own snapshot.world under
// peer_projects[<peerName>][]. Enough for the viewer to render the
// reference (folder header, launch fallback) without proxying a
// separate request. Session counts and last-active timestamps are
// derived client-side from stamped sessions.
type SpokeProject struct {
	Slug      string `json:"slug"`
	LaunchCwd string `json:"launch_cwd,omitempty"`
}

// SpokeDiscovered is the hub's verbatim copy of a single entry from a
// spoke's authoritative `discovered` list (GET /v1/projects). Discovery
// is host-authoritative (ADR 0002/0005): only the owning host runs its
// own match rules over its own sessions, so the hub re-broadcasts the
// spoke's self-advertised discovered rows under
// peer_discovered[<peerName>][] rather than recomputing them blind.
// The shape mirrors projects.DiscoveredProject (and the TS
// DiscoveredProject) field-for-field.
type SpokeDiscovered struct {
	SuggestedSlug string   `json:"suggested_slug"`
	Remote        string   `json:"remote,omitempty"`
	Paths         []string `json:"paths"`
	SessionCount  int      `json:"session_count"`
	ActiveCount   int      `json:"active_count"`
	LastActive    string   `json:"last_active,omitempty"`
}

// PeerInfo is the public status of a single peer connection.
type PeerInfo struct {
	Name            string        `json:"name"`
	URL             string        `json:"url"`
	Status          string        `json:"status"`
	SessionCount    int           `json:"session_count"`
	LastError       string        `json:"last_error,omitempty"`
	Version         string        `json:"version,omitempty"`
	DefaultLauncher string        `json:"default_launcher,omitempty"`
	Launchers       []LauncherDef `json:"launchers,omitempty"`
	// Local is true when this peer is conceptually an extension of
	// the host (a devcontainer discovered by the Docker watcher,
	// not a network peer). Local peers don't own their own project
	// assignments; the parent's match rules stamp their sessions,
	// which then bucket into the parent's local folders. See ADR
	// 0002 amendment.
	Local bool `json:"local,omitempty"`
	// Source records how the peer was added: "tailscale" (auto-discovered
	// on the tailnet), "devcontainer" (auto-discovered Docker container),
	// or "manual" (peers.json / POST /v1/peers). Used by the UI to group
	// hosts; empty for the synthesized self row.
	Source string `json:"source,omitempty"`
	// NodeID is the peer's stable opaque identity (ADR 0007), reported
	// by its /v1/health. The viewer anchors references on it so they
	// survive the peer being renamed. Empty for offline peers we've
	// never probed and for pre-ADR-0007 daemons.
	NodeID string `json:"node_id,omitempty"`
}

// NamespaceID returns a store-key for a remote session: "originalID@peerName".
func NamespaceID(originalID, peerName string) string {
	return originalID + "@" + peerName
}

// ParseID splits a potentially namespaced session ID on the last "@".
// Returns (originalID, peerName). For local sessions (no "@"), peerName
// is empty and originalID is the full input.
func ParseID(namespacedID string) (originalID, peerName string) {
	i := strings.LastIndex(namespacedID, "@")
	if i < 0 {
		return namespacedID, ""
	}
	return namespacedID[:i], namespacedID[i+1:]
}
