package peering

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func TestCategorizeError(t *testing.T) {
	tests := []struct {
		err  string
		want string
	}{
		{"connect: auth failed (HTTP 401)", "authentication failed"},
		{"connect: dial tcp 127.0.0.1:8790: connect: connection refused", "connection refused"},
		{"connect: dial tcp: lookup bad.host: no such host", "host not found"},
		{"connect: context deadline exceeded", "connection timed out"},
		{"connect: dial tcp 10.0.0.1:443: i/o timeout", "connection timed out"},
		{"connect: tls: failed to verify certificate", "TLS certificate error"},
		{"connect: x509: certificate signed by unknown authority", "TLS certificate error"},
		{"no data received", "no data received"},
		{"stream ended", "connection lost"},
		{"read: unexpected EOF", "connection failed"},
	}
	for _, tt := range tests {
		got := categorizeError(errors.New(tt.err))
		if got != tt.want {
			t.Errorf("categorizeError(%q) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// failingTransport fails every request with the error currently in
// errs[0], advancing through errs as attempts arrive (the last error
// repeats). It counts attempts so tests can wait for a deterministic
// number of reconnects instead of sleeping.
type failingTransport struct {
	mu       sync.Mutex
	errs     []error
	attempts int
}

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	i := f.attempts - 1
	if i >= len(f.errs) {
		i = len(f.errs) - 1
	}
	return nil, f.errs[i]
}

func (f *failingTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// runFailingPeer runs a peer whose every connection attempt fails
// with the given errors, captures log output, and returns it once at
// least minAttempts attempts have been made.
func runFailingPeer(t *testing.T, minAttempts int, errs ...error) string {
	t.Helper()

	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	ft := &failingTransport{errs: errs}
	p := newPeer(config.PeerConfig{Name: "down", URL: "http://spoke.invalid"}, store.New(), nil,
		WithTransport(ft))
	p.reconnectBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for ft.count() < minAttempts {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d attempts (got %d)", minAttempts, ft.count())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	return buf.String()
}

// TestRun_DedupesDisconnectLogs verifies that repeated identical
// connection failures produce a single disconnect log line rather
// than one per retry attempt (issue #244).
func TestRun_DedupesDisconnectLogs(t *testing.T) {
	logs := runFailingPeer(t, 5, errors.New("connection refused"))

	got := strings.Count(logs, "peering: down: disconnected:")
	if got != 1 {
		t.Errorf("want exactly 1 disconnect log for repeated identical failures, got %d\nlogs:\n%s", got, logs)
	}
}

// TestRun_LogsAgainWhenErrorChanges verifies that log dedup keys on
// the error text: a different failure logs again rather than being
// swallowed by the dedup of the previous one.
func TestRun_LogsAgainWhenErrorChanges(t *testing.T) {
	logs := runFailingPeer(t, 5,
		errors.New("connection refused"),
		errors.New("connection refused"),
		errors.New("no such host"))

	if got := strings.Count(logs, "peering: down: disconnected:"); got != 2 {
		t.Errorf("want 2 disconnect logs (one per distinct error), got %d\nlogs:\n%s", got, logs)
	}
	if !strings.Contains(logs, "connection refused") || !strings.Contains(logs, "no such host") {
		t.Errorf("want both error texts logged\nlogs:\n%s", logs)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing log output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRun_LogsAgainAfterReconnect verifies that a successful
// connection resets the log dedup (and backoff): the same failure
// occurring again after a reconnect is logged again, not swallowed
// as a duplicate of the pre-reconnect failure.
func TestRun_LogsAgainAfterReconnect(t *testing.T) {
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// A spoke that serves a valid SSE stream but ends it immediately:
	// every attempt connects successfully, then drops with the same
	// "stream ended" error.
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
		// Return immediately: stream ends right after connecting.
	}))
	defer srv.Close()

	p := newPeer(config.PeerConfig{Name: "flappy", URL: srv.URL}, store.New(), nil)
	p.reconnectBackoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for 3 connections (got %d)", attempts.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	// Each drop follows a successful connection, so dedup resets each
	// time: expect one disconnect log per completed connection cycle
	// even though the error text is identical.
	if got := strings.Count(buf.String(), "peering: flappy: disconnected:"); got < 2 {
		t.Errorf("want a disconnect log per post-reconnect drop, got %d\nlogs:\n%s", got, buf.String())
	}
}
