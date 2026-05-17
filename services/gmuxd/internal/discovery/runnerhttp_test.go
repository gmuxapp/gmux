package discovery

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// unixServer wraps an HTTP server on a Unix socket and exposes
// counters for accepted connections and currently-open connections.
// `open` lets tests assert that the server saw a clean close (req
// completed and the connection went away), distinguishing
// req.Close=true from a pooled-but-discarded conn.
type unixServer struct {
	socketPath string
	accepts    atomic.Int64
	open       atomic.Int64 // currently-open server-side conns
	cleanup    func()
}

func startUnixServer(t *testing.T, h http.Handler) *unixServer {
	t.Helper()
	s := &unixServer{socketPath: filepath.Join(t.TempDir(), "test.sock")}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: h,
		ConnState: func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				s.open.Add(1)
			case http.StateClosed, http.StateHijacked:
				s.open.Add(-1)
			}
		},
	}
	counted := &countingListener{Listener: ln, accepts: &s.accepts}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(counted)
		close(done)
	}()
	s.cleanup = func() {
		_ = srv.Close()
		<-done
	}
	return s
}

// waitOpen polls until the server-side open-conn count matches want
// or the test deadline elapses. Conn-state transitions are async on
// the server's goroutines, so a tiny settle is needed after the
// client closes a response body.
func (s *unixServer) waitOpen(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.open.Load() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("open conns = %d, want %d (timed out)", s.open.Load(), want)
}

type countingListener struct {
	net.Listener
	accepts *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

// TestRunnerRequest_ClosesConnectionAfterResponse is the central
// regression for issue #197. It verifies that runnerRequest causes
// the server to see a clean connection close after each request:
// req.Close=true means the underlying FD is released when the
// caller closes the response body, not pooled in an abandoned
// transport that GC may take ages to reach.
//
// If req.Close were dropped, the conn would be pooled in the
// per-call transport. The server keeps it in StateIdle until its
// own keep-alive timeout, so this test would observe a non-zero
// open count after each iteration.
func TestRunnerRequest_ClosesConnectionAfterResponse(t *testing.T) {
	var requests atomic.Int64
	s := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer s.cleanup()

	const N = 5
	for i := 0; i < N; i++ {
		resp, err := runnerRequest(context.Background(), s.socketPath, http.MethodGet, "/probe", nil)
		if err != nil {
			t.Fatalf("runnerRequest[%d]: %v", i, err)
		}
		_ = resp.Body.Close()
		// After closing the body, req.Close=true must have caused
		// the conn to close on the server side. If the conn were
		// pooled (idle), open would stay at 1.
		s.waitOpen(t, 0)
	}

	if got := requests.Load(); got != N {
		t.Fatalf("server saw %d requests, want %d", got, N)
	}
	if got := s.accepts.Load(); got != N {
		t.Fatalf("server saw %d accepts, want %d (each call must dial a new conn)", got, N)
	}
}

// TestRunnerRequest_HonorsCallerContext verifies that a caller's
// context cancellation propagates into the in-flight request and
// short-circuits the runner-side timeout. The helper composes the
// caller's ctx with http.Client.Timeout, so the earlier deadline
// must win; if ctx weren't threaded through (e.g., a future
// regression that drops NewRequestWithContext), a cancelled caller
// would still wait the full 3 s for Client.Timeout.
func TestRunnerRequest_HonorsCallerContext(t *testing.T) {
	// Server hangs forever so the only way out is ctx cancellation.
	hang := make(chan struct{})
	defer close(hang)
	s := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	defer s.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runnerRequest(ctx, s.socketPath, http.MethodGet, "/hang", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled request, got nil")
	}
	// The 3-second runner timeout would otherwise dominate. Any
	// elapsed time well under that proves the caller's ctx won.
	if elapsed > time.Second {
		t.Fatalf("request took %v; expected <1s (caller ctx must short-circuit runner timeout)", elapsed)
	}
}

// TestSendInput_DeliversBodyToRunner is the contract `gmux --send`
// relies on: the bytes the CLI hands gmuxd must arrive at the
// runner's /input verbatim. Anything that mangles them (extra
// framing, trailing newline normalization) would silently corrupt
// keystrokes sent to a session.
func TestSendInput_DeliversBodyToRunner(t *testing.T) {
	var got []byte
	s := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/input" {
			t.Errorf("runner saw method=%s path=%s, want POST /input", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		got = b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.cleanup()

	// A payload with bytes that would be tempting to "normalize"
	// (raw CR, embedded NUL, high-bit). None of these may change.
	want := []byte("hello\rworld\x00\xff!")
	if err := SendInput(context.Background(), s.socketPath, bytes.NewReader(want)); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("runner received %q, want %q", got, want)
	}
}

// TestSendInput_SurfacesRunnerErrors makes sure a non-2xx runner
// response becomes a meaningful error rather than a silent
// success. Otherwise `gmux --send` would print nothing and exit 0
// when the runner rejected the input.
func TestSendInput_SurfacesRunnerErrors(t *testing.T) {
	s := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "write pty: broken pipe", http.StatusInternalServerError)
	}))
	defer s.cleanup()

	err := SendInput(context.Background(), s.socketPath, bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("expected error on 500 from runner, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status, got %v", err)
	}
}
