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
		// F6: a loopback peer is the standard "second daemon on this laptop"
		// setup. A refused loopback socket is an OS fact, not a tailscale one,
		// so arming the probe for it churns a subprocess every TTL window,
		// forever, for the common "my laptop peer is switched off" state.
		{"down loopback (localhost) peer", []peering.PeerInfo{{Name: "laptop", URL: "http://localhost:8791", Status: "disconnected"}}, false},
		{"down 127.0.0.1 peer", []peering.PeerInfo{{Name: "laptop", URL: "http://127.0.0.1:8791", Status: "connecting"}}, false},
		{"down IPv6 loopback peer", []peering.PeerInfo{{Name: "laptop", URL: "http://[::1]:8791", Status: "disconnected"}}, false},
		{"down tailnet peer", []peering.PeerInfo{{Name: "spoke", URL: "https://100.96.12.34:8790", Status: "disconnected"}}, true},
		{"down LAN peer (not loopback)", []peering.PeerInfo{{Name: "spoke", URL: "http://192.168.1.20:8790", Status: "disconnected"}}, true},
		{"loopback peer up, tailnet peer down", []peering.PeerInfo{{Name: "laptop", URL: "http://localhost:8791", Status: "disconnected"}, {Name: "spoke", URL: "https://100.96.12.34", Status: "disconnected"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerLinksDown(tt.peers); got != tt.want {
				t.Errorf("peerLinksDown = %v, want %v", got, tt.want)
			}
		})
	}
}
