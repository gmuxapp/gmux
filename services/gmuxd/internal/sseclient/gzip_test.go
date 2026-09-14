package sseclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/httpz"
)

// TestSubscribe_ThroughGzipMiddleware is the peer-link (hub↔spoke) proof.
// The spoke's network handler is wrapped in httpz.Gzip; the hub's
// sseclient uses a plain net/http transport, which asks for gzip and
// decodes transparently. Two properties must hold for the peer stream to
// stay a *stream*: the server actually negotiated gzip (the wire bytes
// are compressed), and each flushed event reaches the handler before the
// server writes the next one — i.e. Go's transparent decoder does not
// buffer across a sync-flush boundary any more than the browser does.
func TestSubscribe_ThroughGzipMiddleware(t *testing.T) {
	var negotiated atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewServer(httpz.Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		negotiated.Store(httpz.AcceptsGzip(r))
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		fmt.Fprintf(w, "event: snapshot.sessions.batch\ndata: %s\n\n", strings.Repeat(`{"id":"s1"},`, 200))
		_ = rc.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("client never acknowledged the first event")
			return
		}
		fmt.Fprint(w, "event: snapshot.sessions.ready\ndata: {\"epoch\":1}\n\n")
		_ = rc.Flush()
	})))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got []string
	err := New(srv.URL).Subscribe(ctx, nil, func(ev Event) {
		got = append(got, ev.Type)
		if ev.Type == "snapshot.sessions.batch" {
			close(release)
		}
	})
	if err != ErrStreamEnded {
		t.Fatalf("Subscribe: %v", err)
	}
	if !negotiated.Load() {
		t.Fatal("hub transport did not offer gzip; the peer link would not be compressed")
	}
	if strings.Join(got, ",") != "snapshot.sessions.batch,snapshot.sessions.ready" {
		t.Fatalf("events = %v", got)
	}
}
