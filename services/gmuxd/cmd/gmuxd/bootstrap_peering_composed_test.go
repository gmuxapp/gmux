package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

type blockingPeerStatusSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingPeerStatusSink) ReplacePeerSessions(string, []peering.SessionProjection) {}
func (s *blockingPeerStatusSink) RemovePeerSessions(string)                               {}
func (s *blockingPeerStatusSink) SessionActivity(string)                                  {}
func (s *blockingPeerStatusSink) PeerWorldChanged(string)                                 {}
func (s *blockingPeerStatusSink) AliveSessionCount(string) int {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return 0
}

type composedSpoke struct {
	*httptest.Server
	connected chan struct{}
	frames    chan string
	requests  atomic.Int32
	once      sync.Once
}

func newComposedSpoke(t *testing.T, sessions []peering.SessionProjection) *composedSpoke {
	t.Helper()
	s := &composedSpoke{connected: make(chan struct{}), frames: make(chan string, 8)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{"sessions": sessions})
		fmt.Fprintf(w, "event: snapshot.sessions\ndata: %s\n\n", data)
		w.(http.Flusher).Flush()
		s.once.Do(func() { close(s.connected) })
		for {
			select {
			case <-r.Context().Done():
				return
			case frame := <-s.frames:
				fmt.Fprint(w, frame)
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestComposedPeerActivityReachesCoordinatorOutcomeWithoutLegacySink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	coord := sessioncoord.New(nil, nil, st, nil, nil)
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.FinishConvergence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	b := &Bootstrap{Coordinator: coord}
	got := make(chan sessioncoord.Outcome, 1)
	triggersDone := make(chan error, 1)
	go func() {
		triggersDone <- b.StartTriggers(ctx, TriggerConfig{Activity: func(o sessioncoord.Outcome) { got <- o }})
	}()

	spoke := newComposedSpoke(t, nil)
	mgr := peering.NewProjectionManager([]config.PeerConfig{{Name: "box", URL: spoke.URL}}, "self", nil, peering.EventHooks{})
	adapter := &centralPeerAdapter{manager: mgr, store: st, dirty: func(bool, bool) {}, activity: coord.PublishActivity}
	mgr.SetEventHooks(adapter.hooks())
	mgr.Start()
	defer mgr.Stop()
	select {
	case <-spoke.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not connect")
	}
	spoke.frames <- "event: session-activity\ndata: {\"id\":\"s1\"}\n\n"
	select {
	case o := <-got:
		if o.Type != sessioncoord.OutcomeActivity || o.ID != "s1@box" {
			t.Fatalf("outcome=%+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("activity outcome not received")
	}
	cancel()
	select {
	case err := <-triggersDone:
		if err != nil && err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("triggers did not stop")
	}
}

func TestComposedLocalPeerConnectAssignsAndTransientDisconnectPrunes(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.ReplaceProjectCatalog(ctx, []centralstore.ProjectEntrySpec{{Owned: &centralstore.OwnedProjectSpec{Slug: "work", Rules: []centralstore.MatchRule{{Path: "/work"}}}}}, 1); err != nil {
		t.Fatal(err)
	}
	spoke := newComposedSpoke(t, []peering.SessionProjection{{ID: "s1", Alive: true, Cwd: "/work"}})
	mgr := peering.NewProjectionManager([]config.PeerConfig{{Name: "box", URL: spoke.URL, Local: true}}, "self", nil, peering.EventHooks{})
	var mu sync.Mutex
	var dirt [][2]bool
	adapter := &centralPeerAdapter{manager: mgr, store: st, dirty: func(s, w bool) { mu.Lock(); dirt = append(dirt, [2]bool{s, w}); mu.Unlock() }}
	mgr.SetEventHooks(adapter.hooks())
	mgr.Start()
	defer mgr.Stop()
	eventually(t, func() bool {
		snap, _ := st.ReadSnapshot(ctx, centralstore.SnapshotQuery{IncludeProjects: true})
		return len(snap.LocalPeerPlacements) == 1 && len(mgr.SessionProjections()) == 1
	})
	mu.Lock()
	baseline := len(dirt)
	mu.Unlock()
	spoke.CloseClientConnections()
	spoke.Close()
	eventually(t, func() bool {
		snap, _ := st.ReadSnapshot(ctx, centralstore.SnapshotQuery{IncludeProjects: true})
		return len(snap.LocalPeerPlacements) == 0 && len(mgr.SessionProjections()) == 0
	})
	mu.Lock()
	defer mu.Unlock()
	prunes := 0
	for _, d := range dirt[baseline:] {
		if d == [2]bool{true, false} {
			prunes++
		}
	}
	if prunes != 1 {
		t.Fatalf("durable prune dirty notifications=%d, after disconnect=%v", prunes, dirt[baseline:])
	}
}

func TestReconcileManualPeersToleratesConcurrentRemoval(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, _, err := st.UpsertManualPeer(ctx, centralstore.ManualPeerSpec{Name: "box", URL: "http://box", Token: "secret"}, 1); err != nil {
		t.Fatal(err)
	}

	// PeerStatus holds the manager read lock while consulting the sink. Queue a
	// removal there so it runs after reconcile snapshots the runtime but before
	// reconcile ensures every desired config, reproducing the later TOCTOU
	// ordering from the review deterministically.
	sink := &blockingPeerStatusSink{entered: make(chan struct{}), release: make(chan struct{})}
	mgr := peering.NewProjectionManager([]config.PeerConfig{{Name: "box", URL: "http://box", Token: "secret"}}, "self", sink, peering.EventHooks{})
	reconcileDone := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("reconcile panicked: %v", r)
			}
			reconcileDone <- err
		}()
		err = reconcileManualPeers(ctx, st, mgr)
	}()
	<-sink.entered
	removeStarted := make(chan struct{})
	removeDone := make(chan struct{})
	go func() {
		close(removeStarted)
		mgr.RemovePeer("box")
		close(removeDone)
	}()
	<-removeStarted
	// Let RemovePeer queue for the manager lock before reconciliation resumes.
	time.Sleep(10 * time.Millisecond)
	close(sink.release)

	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	<-removeDone
	if p := mgr.GetPeer("box"); p == nil || p.Config.Token != "secret" {
		t.Fatalf("durable peer was not restored after concurrent removal: %v", p)
	}
}

func TestReconcileManualPeersPreStartUsesDurableRowsAndLatestCredentials(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b := newComposedSpoke(t, nil), newComposedSpoke(t, nil)
	if _, _, _, err := st.UpsertManualPeer(ctx, centralstore.ManualPeerSpec{Name: "a", URL: a.URL, Token: "old"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.UpsertManualPeer(ctx, centralstore.ManualPeerSpec{Name: "b", URL: b.URL, Token: "two"}, 2); err != nil {
		t.Fatal(err)
	}
	mgr := peering.NewProjectionManager(nil, "self", nil, peering.EventHooks{})
	if err := reconcileManualPeers(ctx, st, mgr); err != nil {
		t.Fatal(err)
	}
	if a.requests.Load() != 0 || b.requests.Load() != 0 {
		t.Fatal("pre-Start reconciliation launched network goroutines")
	}
	if _, _, _, err := st.UpsertManualPeer(ctx, centralstore.ManualPeerSpec{Name: "a", URL: a.URL, Token: "new"}, 3); err != nil {
		t.Fatal(err)
	}
	if err := reconcileManualPeers(ctx, st, mgr); err != nil {
		t.Fatal(err)
	}
	if p := mgr.GetPeer("a"); p == nil || p.Config.Token != "new" {
		t.Fatalf("credential replacement=%v", p)
	}
	if len(mgr.PeerStatus()) != 2 {
		t.Fatalf("peers=%+v", mgr.PeerStatus())
	}
	mgr.Start()
	defer mgr.Stop()
	eventually(t, func() bool { return a.requests.Load() > 0 && b.requests.Load() > 0 })
}
