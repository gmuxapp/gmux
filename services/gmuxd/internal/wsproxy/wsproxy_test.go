package wsproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// fakeConn is a placeholder *websocket.Conn stand-in. addConn only stores
// the pointer and never touches it, so a zero-value conn is sufficient for
// exercising the per-session cap bookkeeping.
func fakeConn() *websocket.Conn { return &websocket.Conn{} }

func TestAddConnEnforcesPerSessionCap(t *testing.T) {
	p := New(nil, nil)
	const sid = "sess-1"

	// Up to the cap, every connection is accepted.
	for i := 0; i < maxClientsPerSession; i++ {
		if !p.addConn(sid, fakeConn()) {
			t.Fatalf("connection %d was refused below the cap of %d", i+1, maxClientsPerSession)
		}
	}

	// One past the cap is refused without being registered.
	if p.addConn(sid, fakeConn()) {
		t.Fatalf("connection beyond cap of %d was accepted", maxClientsPerSession)
	}

	p.mu.Lock()
	got := len(p.sessions[sid])
	p.mu.Unlock()
	if got != maxClientsPerSession {
		t.Fatalf("registered %d connections, want %d (refused conn must not be stored)", got, maxClientsPerSession)
	}
}

func TestAddConnCapIsPerSession(t *testing.T) {
	p := New(nil, nil)

	// Fill session A to the cap.
	for i := 0; i < maxClientsPerSession; i++ {
		if !p.addConn("A", fakeConn()) {
			t.Fatalf("session A connection %d refused below cap", i+1)
		}
	}
	if p.addConn("A", fakeConn()) {
		t.Fatal("session A accepted a connection beyond its cap")
	}

	// A different session is unaffected: legit multi-device use elsewhere
	// must still work even when one session is saturated.
	if !p.addConn("B", fakeConn()) {
		t.Fatal("session B refused while only session A was at cap")
	}
}

func TestRemoveConnFreesCapSlot(t *testing.T) {
	p := New(nil, nil)
	const sid = "sess-1"

	conns := make([]*websocket.Conn, 0, maxClientsPerSession)
	for i := 0; i < maxClientsPerSession; i++ {
		c := fakeConn()
		conns = append(conns, c)
		p.addConn(sid, c)
	}
	if p.addConn(sid, fakeConn()) {
		t.Fatal("accepted beyond cap before any disconnect")
	}

	// A viewer leaving frees a slot for a reconnecting client.
	p.removeConn(sid, conns[0])
	if !p.addConn(sid, fakeConn()) {
		t.Fatal("connection refused after a slot was freed")
	}
}

// startBackend stands in for a gmux-run session: a minimal WebSocket
// server on a Unix socket that accepts and holds connections open. It
// lets the cap be exercised end-to-end through the real proxy handler.
func startBackend(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		// Hold the connection open until the client/proxy goes away.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
		os.Remove(sockPath)
	})
	return sockPath
}

// TestHandlerRefusesBeyondCapWithCloseReason drives the full HTTP handler:
// it opens maxClientsPerSession proxied connections (which must succeed and
// stay open) and then confirms the next one is refused with the
// StatusTryAgainLater close code and a clear reason, leaving the existing
// viewers untouched (refuse, don't shed the healthy ones).
func TestHandlerRefusesBeyondCapWithCloseReason(t *testing.T) {
	sockPath := startBackend(t)
	p := New(
		func(string) (string, error) { return sockPath, nil },
		nil,
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/{sessionID}", p.Handler())
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)

	wsURL := strings.Replace(front.URL, "http://", "ws://", 1) + "/ws/sess-1"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dial := func() (*websocket.Conn, error) {
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		return c, err
	}

	// Open exactly cap connections; all must succeed and stay open.
	accepted := make([]*websocket.Conn, 0, maxClientsPerSession)
	for i := 0; i < maxClientsPerSession; i++ {
		c, err := dial()
		if err != nil {
			t.Fatalf("dial %d below cap failed: %v", i+1, err)
		}
		accepted = append(accepted, c)
	}
	t.Cleanup(func() {
		for _, c := range accepted {
			c.Close(websocket.StatusNormalClosure, "")
		}
	})

	// Wait until all cap slots are registered (Handler registers
	// asynchronously relative to Dial returning).
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.sessions["sess-1"])
		p.mu.Unlock()
		if n == maxClientsPerSession {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d connections registered", n, maxClientsPerSession)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The next connection must be refused with a clear close reason.
	over, err := dial()
	if err != nil {
		// Some refusals surface as a dial error; that's also acceptable
		// shedding behavior. But the typical path is accept-then-close,
		// which Dial reports as success followed by a Read error.
		return
	}
	defer over.Close(websocket.StatusNormalClosure, "")
	_, _, readErr := over.Read(ctx)
	if readErr == nil {
		t.Fatal("connection beyond cap was not closed")
	}
	var ce websocket.CloseError
	if !errors.As(readErr, &ce) {
		t.Fatalf("expected websocket close error, got %v", readErr)
	}
	if ce.Code != websocket.StatusTryAgainLater {
		t.Fatalf("close code = %d, want %d (StatusTryAgainLater)", ce.Code, websocket.StatusTryAgainLater)
	}
	if !strings.Contains(ce.Reason, "too many connections") {
		t.Fatalf("close reason = %q, want it to mention too many connections", ce.Reason)
	}

	// The existing viewers must still be open — we refuse the newcomer,
	// we don't shed a healthy tab.
	p.mu.Lock()
	n := len(p.sessions["sess-1"])
	p.mu.Unlock()
	if n != maxClientsPerSession {
		t.Fatalf("after refusal %d connections remain, want %d (must not evict healthy viewers)", n, maxClientsPerSession)
	}
}
