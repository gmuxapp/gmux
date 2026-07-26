package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func TestScanRunnerEventsResetsTypeAtFrameBoundary(t *testing.T) {
	out := make(chan sessioncoord.RunnerEvent, 1)
	scanRunnerEvents(context.Background(), strings.NewReader("event: status\n\ndata: {\"working\":true}\n"), out)
	if len(out) != 0 {
		t.Fatal("typeless frame inherited prior event type")
	}
}

func unixRunner(t *testing.T, h http.Handler) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return p
}

type runnerConnTracker struct {
	open atomic.Int64
}

func (c *runnerConnTracker) track(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		c.open.Add(1)
	case http.StateClosed, http.StateHijacked:
		c.open.Add(-1)
	}
}

func trackedUnixRunner(t *testing.T, h http.Handler) (string, *runnerConnTracker) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	tracker := &runnerConnTracker{}
	srv := &http.Server{Handler: h, ConnState: tracker.track}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return p, tracker
}

func waitForRunnerConns(t *testing.T, tracker *runnerConnTracker, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for tracker.open.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open runner connections=%d, want %d", tracker.open.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProductionRunnerMetaClosesConnections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"sess-abc","adapter":"shell","alive":true,"created_at":"2026-01-01T00:00:00Z"}`)
	})
	ep, tracker := trackedUnixRunner(t, mux)
	client := productionRunnerClient{}
	for range 100 {
		if _, err := client.Meta(context.Background(), ep); err != nil {
			t.Fatal(err)
		}
		waitForRunnerConns(t, tracker, 0)
	}
}

func TestProductionRunnerSubscriptionCloseClosesConnection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ep, tracker := trackedUnixRunner(t, mux)
	stream, err := (productionRunnerClient{}).Subscribe(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunnerConns(t, tracker, 1)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	waitForRunnerConns(t, tracker, 0)
}

func TestProductionRunnerSubscribeFirstBuffersPreMeta(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: status\ndata: {\"working\":true}\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"sess-abc","adapter":"shell","alive":true,"created_at":"2026-01-01T00:00:00Z"}`)
	})
	ep := unixRunner(t, mux)
	c := productionRunnerClient{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := c.Subscribe(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := c.Meta(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if m.Registration.ID != "sess-abc" {
		t.Fatalf("meta=%#v", m)
	}
	select {
	case e := <-s.Events():
		if e.Facts.Working == nil || !*e.Facts.Working {
			t.Fatalf("event=%#v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("buffered event lost")
	}
	close(release)
}
func TestProductionRunnerStreamCloseAndCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) { w.(http.Flusher).Flush(); <-r.Context().Done() })
	ep := unixRunner(t, mux)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := (productionRunnerClient{}).Subscribe(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("event after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close")
	}
}
func TestProductionRunnerMalformedMetaAndEvent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: status\ndata: {bad\n\nevent: exit\ndata: {\"exit_code\":3}\n\n")
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{") })
	ep := unixRunner(t, mux)
	s, err := (productionRunnerClient{}).Subscribe(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := <-s.Events()
	if e.Alive == nil || *e.Alive {
		t.Fatalf("event=%#v", e)
	}
	if _, err = (productionRunnerClient{}).Meta(context.Background(), ep); err == nil {
		t.Fatal("malformed meta accepted")
	}
}
func TestProductionRunnerControlUsesKillAndContext(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/kill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	ep := unixRunner(t, mux)
	if err := (productionRunnerControl{}).Terminate(context.Background(), ep); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (productionRunnerControl{}).Terminate(ctx, ep); err == nil {
		t.Fatal("cancel ignored")
	}
}
