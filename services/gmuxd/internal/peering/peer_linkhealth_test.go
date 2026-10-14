package peering

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// TestLinkHealth_NeverConnected drives a peer whose every connect attempt fails
// (a blackholed tailscale IP, a down host behind an expired local key — the
// operator's case) and asserts the lifecycle flags that make it obvious:
// never_connected, a growing consecutive-failure count, no last success.
func TestLinkHealth_NeverConnected(t *testing.T) {
	ft := &failingTransport{errs: []error{errors.New("dial tcp 100.96.12.34:8790: i/o timeout")}}
	p := newPeer(config.PeerConfig{Name: "deadnode", URL: "http://spoke.invalid"}, newMockSink(), nil,
		WithTransport(ft))
	p.reconnectBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()

	waitForAttempts(t, ft, 3)
	cancel()
	<-done

	cf, ever, ls := p.LinkHealth()
	if ever {
		t.Fatalf("everConnected = true; a peer that only failed dials must never be marked connected")
	}
	if !ls.IsZero() {
		t.Errorf("lastSuccess = %v, want zero for a peer that never connected", ls)
	}
	// The 3rd RoundTrip can be in-flight when we cancel, so >=3 attempts
	// guarantees >=2 recorded failures. The point is the run grows and the
	// peer is flagged never-connected, not an exact count.
	if cf < 2 {
		t.Errorf("consecutiveFailures = %d, want >= 2 after >=3 failed attempts", cf)
	}
}

// TestLinkHealth_FlappingPeerIsNotNeverConnected drives a peer that connects
// successfully then drops repeatedly (the flappy spoke). It must NOT be flagged
// never_connected: it is reachable, lastSuccess is set, and a drop of an
// established connection is not counted as a link failure (success resets the
// run, and the drop path has wasConnected == true).
func TestLinkHealth_FlappingPeerIsNotNeverConnected(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "{}")
			return
		}
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		// Stream ends immediately after connecting → connect succeeds, drops.
	}))
	defer srv.Close()

	p := newPeer(config.PeerConfig{Name: "flappy", URL: srv.URL}, newMockSink(), nil)
	p.reconnectBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for 3 connections (got %d)", attempts.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	cf, ever, ls := p.LinkHealth()
	if !ever {
		t.Fatalf("everConnected = false; a peer that connected then dropped has connected")
	}
	if ls.IsZero() {
		t.Errorf("lastSuccess = zero; a peer that connected has a last success time")
	}
	// Drops of an established link are not counted as link failures; success
	// resets the run, so a flapping-but-reachable peer shows a low/zero count —
	// visually distinct from the never-connected storm.
	if cf > 1 {
		t.Errorf("consecutiveFailures = %d; drops of an established connection must not accumulate a failure run", cf)
	}
}

// TestPeerStatus_CarriesLinkHealth asserts the PeerInfo wire projection carries
// the lifecycle fields, so /v1/health and daemon status can render them.
func TestPeerStatus_CarriesLinkHealth(t *testing.T) {
	ft := &failingTransport{errs: []error{errors.New("connection refused")}}
	mgr := NewProjectionManager([]config.PeerConfig{{Name: "dead", URL: "http://spoke.invalid", Source: config.SourceManual}}, "self", newMockSink(), EventHooks{}, WithTransport(ft))
	for _, mp := range mgr.peers {
		mp.peer.reconnectBackoff = time.Millisecond
	}
	mgr.Start()
	defer mgr.Stop()

	waitForAttempts(t, ft, 2)

	var info PeerInfo
	for _, p := range mgr.PeerStatus() {
		if p.Name == "dead" {
			info = p
		}
	}
	if !info.NeverConnected {
		t.Fatalf("PeerInfo.NeverConnected = false; a never-connecting peer must be flagged")
	}
	if info.ConsecutiveFailures < 1 {
		t.Errorf("PeerInfo.ConsecutiveFailures = %d, want >= 1", info.ConsecutiveFailures)
	}
	if info.LastSuccess != "" {
		t.Errorf("PeerInfo.LastSuccess = %q, want empty for a never-connected peer", info.LastSuccess)
	}
}
