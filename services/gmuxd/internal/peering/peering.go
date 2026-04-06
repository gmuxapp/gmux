// Package peering manages connections to remote gmuxd instances (spokes).
//
// A hub gmuxd subscribes to each spoke's GET /v1/events SSE stream,
// transforms remote sessions into the local store with namespaced IDs
// (originalID@peerName), and tracks connection state per peer.
//
// Remote session IDs use the convention "originalID@peerName". The hub
// splits on the last "@" to route actions and WebSocket connections back
// to the owning spoke.
package peering

import "strings"

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

