package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

type barrierReconciler struct {
	calls chan []sessioncoord.ReconcileCandidate
	count atomic.Int32
}

func (r *barrierReconciler) ReconcileRetained(ctx context.Context, _ string, batch []sessioncoord.ReconcileCandidate) ([]sessioncoord.ReconcileDecision, error) {
	r.count.Add(1)
	copyBatch := append([]sessioncoord.ReconcileCandidate(nil), batch...)
	select {
	case r.calls <- copyBatch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]sessioncoord.ReconcileDecision, len(batch))
	for i, c := range batch {
		out[i] = sessioncoord.ReconcileDecision{ID: c.ID, Disposition: sessioncoord.DispositionRetain}
	}
	return out, nil
}

type controlledEndpoints struct {
	mu    sync.RWMutex
	eps   []string
	calls chan []string
}
type barrierControl struct{ calls chan string }

func (c *barrierControl) Terminate(ctx context.Context, endpoint string) error {
	select {
	case c.calls <- endpoint:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *controlledEndpoints) set(eps ...string) {
	s.mu.Lock()
	s.eps = append([]string(nil), eps...)
	s.mu.Unlock()
}
func (s *controlledEndpoints) Endpoints(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	out := append([]string(nil), s.eps...)
	s.mu.RUnlock()
	select {
	case s.calls <- out:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return out, nil
}

func awaitReconcile(t *testing.T, ch <-chan []sessioncoord.ReconcileCandidate) []sessioncoord.ReconcileCandidate {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Second):
		t.Fatal("reconcile trigger did not run")
		return nil
	}
}

type frameKinds struct{ sessions, world bool }

func awaitFrame(t *testing.T, ch <-chan frameKinds, want frameKinds) {
	t.Helper()
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("composition %v did not run", want)
		}
	}
}

func TestStartTriggersFullComposedGraphAndJoinedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	exited := centralstore.UnixMillis(5)
	if _, _, err = st.InsertSession(ctx, centralstore.NewSession{ID: "sess-retained-trigger", Adapter: "shell", ConversationRef: "conv", CWD: "/tmp", Command: []string{"sh"}, CreatedAt: 1, ExitedAt: &exited}); err != nil {
		t.Fatal(err)
	}
	fleet := newHarnessFleet(3)
	// runner-000 is the startup generation which the later scan must reap;
	// runner-001 is discovered by that scan; runner-002 reports a death.
	// The periodic candidate claims the startup session ID, making it an
	// orphan generation for ReapOrphans after its registration is rejected.
	scanMeta := fleet.metas["runner-001"]
	scanMeta.Registration.ID = fleet.metas["runner-000"].Registration.ID
	fleet.metas["runner-001"] = scanMeta
	dead := fleet.metas["runner-002"]
	dead.Registration.Alive = false
	dead.Registration.ID = "sess-death-trigger"
	fleet.metas["runner-002"] = dead
	reconciler := &barrierReconciler{calls: make(chan []sessioncoord.ReconcileCandidate, 16)}
	endpoints := &controlledEndpoints{calls: make(chan []string, 8)}
	endpoints.set("runner-000")
	frames := make(chan frameKinds, 32)
	control := &barrierControl{calls: make(chan string, 4)}
	var callbacks atomic.Int32
	b, err := newBootstrap(BootstrapConfig{Store: st, Runners: fleet, Control: control, Reconciler: reconciler, Converter: &wire.Converter{}, Endpoints: endpoints, Frames: func(_ context.Context, f wire.Frames) {
		callbacks.Add(1)
		frames <- frameKinds{f.Sessions != nil, f.World != nil}
	}, Clock: func() centralstore.UnixMillis { return 100 }, RunnerBudget: time.Second, ConvergeDeadline: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Converge(ctx); err != nil {
		t.Fatal(err)
	}
	<-endpoints.calls
	composerDone := make(chan struct{})
	go func() { b.Composer.Run(ctx); close(composerDone) }()
	conversationDeleted := make(chan struct{}, 4)
	peerSessions := make(chan struct{}, 4)
	peerWorld := make(chan struct{}, 4)
	ticks := make(chan time.Time, 4)
	triggerDone := make(chan error, 1)
	go func() {
		triggerDone <- b.StartTriggers(ctx, TriggerConfig{Tick: ticks, ConversationDeleted: conversationDeleted, PeerSessionsChanged: peerSessions, PeerWorldChanged: peerWorld})
	}()

	// A dead runner outcome takes the coordinator subscription path and causes
	// reconciliation. The first retain verdict changes the runtime overlay and
	// therefore independently dirties sessions.
	if _, err = b.Coordinator.Register(ctx, sessioncoord.RegisterRequest{Endpoint: "runner-002"}); err != nil {
		t.Fatal(err)
	}
	if got := awaitReconcile(t, reconciler.calls); len(got) == 0 {
		t.Fatal("death reconciliation had no retained candidate")
	}
	awaitFrame(t, frames, frameKinds{sessions: true})

	conversationDeleted <- struct{}{}
	if got := awaitReconcile(t, reconciler.calls); len(got) == 0 {
		t.Fatal("deletion reconciliation had no candidate")
	}

	peerSessions <- struct{}{}
	awaitFrame(t, frames, frameKinds{sessions: true})
	peerWorld <- struct{}{}
	// A world composition intentionally re-emits sessions because project
	// placement indices ride on the sessions payload.
	awaitFrame(t, frames, frameKinds{sessions: true, world: true})

	endpoints.set("runner-001")
	ticks <- time.Now()
	select {
	case got := <-endpoints.calls:
		if len(got) != 1 || got[0] != "runner-001" {
			t.Fatalf("scan endpoints=%v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic endpoint scan did not run")
	}
	select {
	case ep := <-control.calls:
		if ep != "runner-001" {
			t.Fatalf("reaped endpoint=%q", ep)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic orphan reap did not run")
	}
	awaitReconcile(t, reconciler.calls)
	regs := b.Registry.Snapshot()
	if len(regs) != 1 || regs[0].Endpoint != "runner-000" {
		t.Fatalf("orphan displaced winner: registry=%+v", regs)
	}

	cancel()
	select {
	case err := <-triggerDone:
		if err != context.Canceled {
			t.Fatalf("StartTriggers=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("trigger workers did not join")
	}
	select {
	case <-composerDone:
	case <-time.After(time.Second):
		t.Fatal("composer did not join")
	}
	before, reconcileBefore := callbacks.Load(), reconciler.count.Load()
	conversationDeleted <- struct{}{}
	peerSessions <- struct{}{}
	peerWorld <- struct{}{}
	ticks <- time.Now()
	if callbacks.Load() != before || reconciler.count.Load() != reconcileBefore {
		t.Fatalf("callback after joined cancellation: frames %d->%d reconciles %d->%d", before, callbacks.Load(), reconcileBefore, reconciler.count.Load())
	}
}
