package main

import (
	"context"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsdiag"
)

// TestUpstreamAccessFor_GateIsTheContract is the daemon-side test for the
// tailscale probe gate — the wiring the PR ships and that previously had NO
// test, so disabling the gate entirely (`if false && peerLinksDown(...)`) kept
// every suite green and probing on every health call was equally invisible.
//
// It pins the three properties the "a healthy hub pays nothing" promise rests
// on: the probe is not called when nothing is down, it IS called when a network
// peer is down, and an inconclusive probe publishes nothing.
func TestUpstreamAccessFor_GateIsTheContract(t *testing.T) {
	ok := func(context.Context) tsdiag.Status {
		return tsdiag.Status{State: tsdiag.StateOK, Detail: "key valid"}
	}

	tests := []struct {
		name      string
		probe     func(context.Context) tsdiag.Status
		peers     []peering.PeerInfo
		wantCalls int
		wantNil   bool
	}{
		{
			name:      "healthy link: probe never runs",
			probe:     ok,
			peers:     []peering.PeerInfo{{Name: "a", Status: "connected"}},
			wantCalls: 0, // M7/M8: a gate mutation shows up here as 1
			wantNil:   true,
		},
		{
			name:      "no peers: probe never runs",
			probe:     ok,
			peers:     nil,
			wantCalls: 0,
			wantNil:   true,
		},
		{
			name:      "down local (devcontainer) peer: probe never runs",
			probe:     ok,
			peers:     []peering.PeerInfo{{Name: "dc", Status: "disconnected", Local: true}},
			wantCalls: 0,
			wantNil:   true,
		},
		{
			name:      "down loopback peer: probe never runs",
			probe:     ok,
			peers:     []peering.PeerInfo{{Name: "laptop", URL: "http://localhost:8791", Status: "disconnected"}},
			wantCalls: 0,
			wantNil:   true,
		},
		{
			name: "down network peer: probe runs and publishes",
			probe: func(context.Context) tsdiag.Status {
				return tsdiag.Status{State: tsdiag.StateKeyExpired, Detail: "EXPIRED", Hint: "run `tailscale up`", KeyExpiry: "2026-05-05T10:00:00Z"}
			},
			peers:     []peering.PeerInfo{{Name: "spoke", URL: "https://100.96.12.34:8790", Status: "connecting"}},
			wantCalls: 1,
			wantNil:   false,
		},
		{
			// F1's waiter path: a wedged probe must degrade to NO diagnostic,
			// never to a diagnosis the daemon has no evidence for.
			name: "inconclusive probe publishes nothing",
			probe: func(context.Context) tsdiag.Status {
				return tsdiag.Status{State: tsdiag.StateUnknown, Detail: "probe in progress"}
			},
			peers:     []peering.PeerInfo{{Name: "spoke", URL: "https://100.96.12.34:8790", Status: "disconnected"}},
			wantCalls: 1,
			wantNil:   true,
		},
		{
			name:      "no prober wired: probe never runs",
			probe:     nil,
			peers:     []peering.PeerInfo{{Name: "spoke", Status: "disconnected"}},
			wantCalls: 0,
			wantNil:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := 0
			var counted func(context.Context) tsdiag.Status
			if tt.probe != nil {
				probe := tt.probe
				counted = func(ctx context.Context) tsdiag.Status {
					n++
					return probe(ctx)
				}
			}
			got := upstreamAccessFor(counted, tt.peers)
			if n != tt.wantCalls {
				t.Errorf("probe called %d times, want %d", n, tt.wantCalls)
			}
			if (got == nil) != tt.wantNil {
				t.Fatalf("upstream_access = %+v, wantNil=%v", got, tt.wantNil)
			}
		})
	}

	t.Run("the caller's context is bounded, not Background", func(t *testing.T) {
		// The gate must hand the probe a deadline: on the health handler and
		// the single composer goroutine, a Background-context probe turns one
		// wedged tailscale CLI into a stall for every client.
		probed, deadline := 0, false
		upstreamAccessFor(func(ctx context.Context) tsdiag.Status {
			probed++
			_, hasDeadline := ctx.Deadline()
			deadline = hasDeadline
			return tsdiag.Status{State: tsdiag.StateOK}
		}, []peering.PeerInfo{{Name: "spoke", URL: "https://100.96.12.34:8790", Status: "disconnected"}})
		if probed != 1 {
			t.Fatalf("probe called %d times, want 1", probed)
		}
		if !deadline {
			t.Error("the probe context has no deadline: health/composer can be stalled by one wedged probe")
		}
	})

	t.Run("the diagnostic is carried through verbatim", func(t *testing.T) {
		probes := 0
		got := upstreamAccessFor(func(context.Context) tsdiag.Status {
			probes++
			return tsdiag.Status{State: tsdiag.StateKeyExpired, Detail: "EXPIRED", Hint: "run `tailscale up`", BackendState: "Running", KeyExpiry: "2026-05-05T10:00:00Z"}
		}, []peering.PeerInfo{{Name: "spoke", URL: "https://100.96.12.34", Status: "disconnected"}})
		if got == nil || got.State != "key_expired" || got.KeyExpiry != "2026-05-05T10:00:00Z" || got.BackendState != "Running" {
			t.Fatalf("diagnostic not projected: %+v", got)
		}
		if probes != 1 {
			t.Errorf("probe called %d times, want 1", probes)
		}
	})
}

// TestPeerURIsLoopback pins F6: which peer hosts must NOT arm the tailscale
// probe. Loopback reachability is an OS fact, not a tailnet one.
func TestPeerURIsLoopback(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"http://localhost:8791", true},
		{"http://LocalHost:8791", true},
		{"http://127.0.0.1:8791", true},
		{"http://127.1.2.3:8791", true}, // the whole 127.0.0.0/8 block is loopback
		{"http://[::1]:8791", true},
		{"http://foo.localhost:8791", true},
		{"http://foo.localhost.", true},
		{"https://100.96.12.34:8790", false}, // tailnet CGNAT
		{"https://laptop.ts.net", false},     // MagicDNS
		{"http://192.168.1.20:8790", false},  // LAN
		{"http://10.0.0.5:8790", false},
		{"", false},
		{"not a url", false},
	}
	for _, tt := range tests {
		if got := peerURIsLoopback(tt.url); got != tt.want {
			t.Errorf("peerURIsLoopback(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}
