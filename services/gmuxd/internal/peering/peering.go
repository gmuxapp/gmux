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

// PeerInfo is the public status of a single peer connection.
type PeerInfo struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	Status       string `json:"status"`
	SessionCount int    `json:"session_count"`
	LastError    string `json:"last_error,omitempty"`
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
