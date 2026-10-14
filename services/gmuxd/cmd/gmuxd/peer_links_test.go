package main

import (
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
)

// TestPeerLinksDown gates the tailscale probe so a healthy hub and a plain-LAN
// box never pay for the shell-out. A peer link is "down" only for a non-local
// peer that is not connected: local (devcontainer) peers are excluded because
// their reachability is not a tailscale concern.
func TestPeerLinksDown(t *testing.T) {
	tests := []struct {
		name  string
		peers []peering.PeerInfo
		want  bool
	}{
		{"no peers", nil, false},
		{"all connected", []peering.PeerInfo{{Name: "a", Status: "connected"}}, false},
		{"a disconnected network peer", []peering.PeerInfo{{Name: "a", Status: "connected"}, {Name: "b", Status: "disconnected"}}, true},
		{"a peer stuck connecting", []peering.PeerInfo{{Name: "a", Status: "connecting"}}, true},
		{"only a down local peer (devcontainer)", []peering.PeerInfo{{Name: "dc", Status: "disconnected", Local: true}}, false},
		{"connected network peer + down local peer", []peering.PeerInfo{{Name: "a", Status: "connected"}, {Name: "dc", Status: "disconnected", Local: true}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerLinksDown(tt.peers); got != tt.want {
				t.Errorf("peerLinksDown = %v, want %v", got, tt.want)
			}
		})
	}
}
