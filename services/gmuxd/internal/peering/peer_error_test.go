package peering

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
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

// TestRun_DedupesDisconnectLogs verifies that repeated identical
// connection failures produce a single disconnect log line rather
// than one per retry attempt (issue #244).
func TestRun_DedupesDisconnectLogs(t *testing.T) {
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// Point at a port nothing listens on: every attempt fails the
	// same way (connection refused).
	p := newPeer(config.PeerConfig{Name: "down", URL: "http://127.0.0.1:1"}, store.New(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.run(ctx)
		close(done)
	}()

	// Enough time for several attempts (backoff starts at 1s).
	time.Sleep(2500 * time.Millisecond)
	cancel()
	<-done

	got := strings.Count(buf.String(), "peering: down: disconnected:")
	if got != 1 {
		t.Errorf("want exactly 1 disconnect log for repeated identical failures, got %d\nlogs:\n%s", got, buf.String())
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
