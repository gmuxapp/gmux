package peering

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
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

// TestLinkHealth_CountersTrackTheRealLifecycle is the deterministic lifecycle
// test for the two guarantees this PR sells: "success resets the run" and "a
// drop of an established link is not counted". A scripted transport drives
// fail → fail → fail → CONNECT → drop → fail → fail → CONNECT, and every
// transition point is asserted, so the counter trajectory is pinned as
// 0 → 1 → 2 → 3 → 0 → 0 → 1 → 2 → 0.
//
// The previous "flapping" fixture had every attempt CONNECT, so recordFailure
// was never called and its `cf > 1` assertion was vacuous: dropping the reset
// in recordSuccess, and counting established-link drops in the run loop, both
// survived the whole suite.
func TestLinkHealth_CountersTrackTheRealLifecycle(t *testing.T) {
	ft := newScriptedTransport(
		// Three failed dials, a connect that then drops, two more failed dials,
		// and a final connect that stays up (the run loop parks inside it).
		fail("connection refused"), fail("connection refused"), fail("i/o timeout"),
		drop, fail("connection refused"), fail("connection refused"), connect)
	p := newPeer(config.PeerConfig{Name: "lifecycle", URL: "http://spoke.invalid"}, newMockSink(), nil,
		WithTransport(ft))
	ft.link = p.LinkHealth
	p.reconnectBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()

	// Each step is observed AFTER it settles: the transport publishes the
	// expected (consecutiveFailures, everConnected) for the next attempt, and
	// the test samples LinkHealth at that point. The final state is sampled
	// after the last connect.
	want := []struct {
		cf   int
		ever bool
	}{
		{0, false}, // before attempt 1: nothing has happened
		{1, false}, // after the first failed dial
		{2, false},
		{3, false}, // the never-connected storm, counted
		{0, true},  // CONNECT resets the run; the peer has now connected
		{1, true},  // the drop DID count nothing; this is the next failed dial
		{2, true},  // counted from the reset baseline, never from before the success
	}
	for i, w := range want {
		select {
		case got := <-ft.samples:
			if got.cf != w.cf || got.ever != w.ever {
				t.Fatalf("after step %d: (cf=%d, ever=%v), want (cf=%d, ever=%v)", i, got.cf, got.ever, w.cf, w.ever)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for lifecycle sample %d", i)
		}
	}

	// Let the final connect settle, then assert the resting state.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cf, ever, ls := p.LinkHealth(); cf == 0 && ever && !ls.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			cf, ever, ls := p.LinkHealth()
			t.Fatalf("after the final connect: cf=%d ever=%v lastSuccess=%v; want cf=0 ever=true with a timestamp", cf, ever, ls)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run loop did not exit after cancel (the scripted stream must close with its ctx)")
	}
}

// linkStep is one scripted outcome for a /v1/events attempt.
type linkStep struct {
	kind string // "fail", "connect", "drop"
	err  string
}

func fail(msg string) linkStep { return linkStep{kind: "fail", err: msg} }

var (
	// connect: SSE opens and STAYS open (a link that is up).
	connect = linkStep{kind: "connect"}
	// drop: SSE opens, then the stream ends (a drop of an established link).
	drop = linkStep{kind: "drop"}
)

// linkSample is the observable link state at a scripted step boundary.
type linkSample struct {
	cf   int
	ever bool
}

// scriptedTransport scripts the exact connect/fail sequence a reconnect loop
// sees, publishing a LinkHealth sample at every /v1/events attempt boundary so
// the counter trajectory can be asserted deterministically (no sleeps, no
// polling races). Non-event requests (the health/projects fetches a connect
// triggers) answer generically and never consume a script step.
type scriptedTransport struct {
	script  []linkStep
	link    func() (int, bool, time.Time)
	mu      sync.Mutex
	i       int
	samples chan linkSample
}

// newScriptedTransport builds a transport whose samples channel exists before
// any attempt runs, so a test can select on it without racing the first dial.
func newScriptedTransport(script ...linkStep) *scriptedTransport {
	return &scriptedTransport{script: script, samples: make(chan linkSample, 16)}
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/v1/events" {
		return jsonResponse("{}"), nil
	}

	s.mu.Lock()
	i := s.i
	if i < len(s.script) {
		s.i++
	}
	step := s.script[min(i, len(s.script)-1)]
	s.mu.Unlock()

	// Sample the peer's counters as this attempt begins: everything the
	// previous attempt did (recordSuccess / recordFailure) has already landed.
	if s.link != nil {
		cf, ever, _ := s.link()
		select {
		case s.samples <- linkSample{cf: cf, ever: ever}:
		default:
		}
	}

	switch step.kind {
	case "fail":
		return nil, errors.New(step.err)
	case "drop":
		// Stream opens (onConnected fires, the run loop marks success) and then
		// EOFs immediately: an established link dropping.
		return sseResponse(strings.NewReader("")), nil
	default: // "connect": an open stream that stays open until the ctx dies
		body, _ := io.Pipe()
		// Transport contract: cancelling the request must terminate the body,
		// or the run loop parks in Read forever and the peer never exits.
		go func() {
			<-req.Context().Done()
			body.CloseWithError(req.Context().Err())
		}()
		return sseResponse(body), nil
	}
}

func sseResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(body),
		Request:    &http.Request{Method: http.MethodGet},
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet},
	}
}

func sseResponseReader(r io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(r),
		Request:    &http.Request{},
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
