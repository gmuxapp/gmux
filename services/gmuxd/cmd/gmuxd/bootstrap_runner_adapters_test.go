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

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func TestScanRunnerEventsResetsTypeAtFrameBoundary(t *testing.T) {
	out := make(chan sessioncoord.RunnerEvent, 1)
	scanRunnerEvents(context.Background(), strings.NewReader("event: status\n\ndata: {\"active\":true}\n"), out)
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

func TestProductionRunnerMetaPreservesLaunchParent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"child","adapter":"pi","alive":true,"created_at":"2026-01-01T00:00:00Z","parent_session_id":"parent"}`)
	})
	meta, err := (productionRunnerClient{}).Meta(context.Background(), unixRunner(t, mux))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Registration.ParentSessionID == nil || *meta.Registration.ParentSessionID != "parent" {
		t.Fatalf("launch parent lost at runner boundary: %#v", meta.Registration.ParentSessionID)
	}
}

func TestRunnerEventProjectionKeepsLifetimeStatusAndExitAtomic(t *testing.T) {
	ev, ok := runnerEventProjection("exit", []byte(`{"exit_code":0,"exited_at":"2026-01-02T03:04:05Z","active":false,"error":false,"interrupted":false,"unread":true,"unread_token":"result-1"}`))
	if !ok {
		t.Fatal("fused lifetime exit was rejected")
	}
	if ev.Alive == nil || *ev.Alive || ev.Facts.Active == nil || *ev.Facts.Active ||
		ev.Facts.Error == nil || *ev.Facts.Error || ev.Facts.Interrupted == nil || *ev.Facts.Interrupted ||
		ev.Facts.Unread == nil || !*ev.Facts.Unread || ev.Facts.UnreadToken == nil || *ev.Facts.UnreadToken != "result-1" ||
		ev.Facts.ExitCode.Set == nil || *ev.Facts.ExitCode.Set != 0 || ev.Facts.ExitedAt.Set == nil {
		t.Fatalf("fused lifetime exit facts lost: %+v", ev)
	}
}

func TestProductionRunnerDeadMetadataSurvivesMetaAndReplay(t *testing.T) {
	const exitedAt = "2026-01-02T03:04:05Z"
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"id":"dead","adapter":"shell","alive":false,"created_at":"2026-01-01T00:00:00Z","exit_code":23,"exited_at":%q}`, exitedAt)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "event: exit\ndata: {\"exit_code\":23,\"exited_at\":%q}\n\n", exitedAt)
	})
	ep := unixRunner(t, mux)
	meta, err := (productionRunnerClient{}).Meta(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	wantExitedAt := centralstore.UnixMillis(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli())
	facts := meta.Registration.Facts
	if facts.ExitCode.Set == nil || *facts.ExitCode.Set != 23 || facts.ExitedAt.Set == nil || *facts.ExitedAt.Set != wantExitedAt {
		t.Fatalf("/meta exit facts lost: %+v", facts)
	}

	stream, err := (productionRunnerClient{}).Subscribe(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event := <-stream.Events()
	if event.Alive == nil || *event.Alive || event.Facts.ExitCode.Set == nil || *event.Facts.ExitCode.Set != 23 || event.Facts.ExitedAt.Set == nil || *event.Facts.ExitedAt.Set != wantExitedAt {
		t.Fatalf("SSE replay exit facts lost: %+v", event)
	}
}

func TestRunnerMetaFactsTreatsUnboundConversationMetadataAsUnobserved(t *testing.T) {
	f := runnerMetaFacts(runnerMetaWire{
		CWD: "/work", WorkspaceRoot: "/work", ShellTitle: "",
		Command: []string{"pi"}, Remotes: map[string]string{},
	})
	if f.ConversationRef != nil || f.AdapterTitle != nil || f.Subtitle != nil || f.Slug != nil {
		t.Fatalf("empty pre-hook metadata became authoritative patches: %+v", f)
	}
	// Shell title is generation-local rather than conversation-local: an
	// empty replacement-runner snapshot deliberately clears the old OSC title.
	if f.ShellTitle == nil || *f.ShellTitle != "" {
		t.Fatalf("shell title lost generation-local clear semantics: %+v", f)
	}

	f = runnerMetaFacts(runnerMetaWire{
		ConversationRef: "/conversations/a.jsonl", AdapterTitle: "Fix auth",
		Subtitle: "working", Slug: "fix-auth", ShellTitle: "shell",
		Command: []string{"pi"}, Remotes: map[string]string{},
	})
	if f.ConversationRef == nil || *f.ConversationRef != "/conversations/a.jsonl" ||
		f.AdapterTitle == nil || *f.AdapterTitle != "Fix auth" ||
		f.Subtitle == nil || *f.Subtitle != "working" ||
		f.Slug == nil || *f.Slug != "fix-auth" {
		t.Fatalf("positive conversation metadata not projected: %+v", f)
	}

	// A bound snapshot is authoritative as a whole. Empty values here are
	// clears that may have happened while gmuxd was disconnected.
	f = runnerMetaFacts(runnerMetaWire{
		ConversationRef: "/conversations/a.jsonl",
		Command:         []string{"pi"}, Remotes: map[string]string{},
	})
	if f.AdapterTitle == nil || *f.AdapterTitle != "" ||
		f.Subtitle == nil || *f.Subtitle != "" ||
		f.Slug == nil || *f.Slug != "" {
		t.Fatalf("bound empty metadata did not project authoritative clears: %+v", f)
	}
}

func TestProductionRunnerMetaClosesConnections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"1j6y9mx6","adapter":"shell","alive":true,"created_at":"2026-01-01T00:00:00Z"}`)
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
		fmt.Fprint(w, "event: status\ndata: {\"active\":true}\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"1j6y9mx6","adapter":"shell","alive":true,"created_at":"2026-01-01T00:00:00Z"}`)
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
	if m.Registration.ID != "1j6y9mx6" {
		t.Fatalf("meta=%#v", m)
	}
	select {
	case e := <-s.Events():
		if e.Facts.Active == nil || !*e.Facts.Active {
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
	if err := (productionRunnerControl{}).Terminate(context.Background(), ep, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (productionRunnerControl{}).Terminate(ctx, ep, ""); err == nil {
		t.Fatal("cancel ignored")
	}
}

// The runner protocol's header names, pinned as literals. The runner lives in
// another module (cli/gmux, ptyserver.IncarnationHeader /
// ExpectIncarnationHeader, pinned by its own test), so the two sides cannot
// share a constant; they can only agree on the wire.
func TestRunnerIncarnationHeaderNamesAreStable(t *testing.T) {
	if runnerIncarnationHeader != "X-Gmux-Incarnation" {
		t.Errorf("runnerIncarnationHeader = %q; the runner stamps X-Gmux-Incarnation", runnerIncarnationHeader)
	}
}
