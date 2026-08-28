package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func newParentNotifyTestRouter(t *testing.T) *centralNotifyRouter {
	t.Helper()
	r := newCentralNotifyRouter(presence.New(presence.Callbacks{}), notifyConfig{
		GracePeriod:   time.Hour,
		IdleThreshold: time.Minute,
	})
	t.Cleanup(r.CancelAllPending)
	return r
}

func notifyRow(adapterName string, active bool, parent string, promoted bool) centralstore.Session {
	row := centralstore.Session{Adapter: adapterName, Active: active}
	if parent != "" {
		id := centralstore.SessionID(parent)
		row.LaunchedFromSessionID = &id
		if !promoted {
			row.ParentSessionID = &id
		}
	}
	return row
}

func hasPendingNotification(r *centralNotifyRouter, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pending[id]
	return ok
}

type readCommitFenceStore struct {
	*centralstore.Store
	committed chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *readCommitFenceStore) RegisterRunner(ctx context.Context, reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
	row, result, err := s.Store.RegisterRunner(ctx, reg)
	if err == nil {
		s.once.Do(func() { close(s.committed) })
		<-s.release
	}
	return row, result, err
}

// These cases drive the committed-outcome consumer directly. Their ordering is
// the race contract: a child's close observes parent outcomes handled before
// it; parent outcomes handled afterward do not revise that close's decision.
func TestCentralNotifyDirectParentSuppression(t *testing.T) {
	tests := []struct {
		name string
		seed []struct {
			id  string
			row centralstore.Session
		}
		child       centralstore.Session
		wantPending bool
	}{
		{
			name:        "root",
			child:       notifyRow("pi", true, "", false),
			wantPending: true,
		},
		{
			name: "active direct parent",
			seed: []struct {
				id  string
				row centralstore.Session
			}{{"parent", notifyRow("pi", true, "", false)}},
			child:       notifyRow("pi", true, "parent", false),
			wantPending: false,
		},
		{
			name: "inactive direct parent",
			seed: []struct {
				id  string
				row centralstore.Session
			}{{"parent", notifyRow("pi", false, "", false)}},
			child:       notifyRow("pi", true, "parent", false),
			wantPending: true,
		},
		{
			name: "active grandparent with inactive direct parent",
			seed: []struct {
				id  string
				row centralstore.Session
			}{
				{"grandparent", notifyRow("pi", true, "", false)},
				{"parent", notifyRow("pi", false, "grandparent", false)},
			},
			child:       notifyRow("pi", true, "parent", false),
			wantPending: true,
		},
		{
			name: "promoted root does not use launch parent",
			seed: []struct {
				id  string
				row centralstore.Session
			}{{"parent", notifyRow("pi", true, "", false)}},
			child: func() centralstore.Session {
				row := notifyRow("pi", true, "", true)
				parent := centralstore.SessionID("parent")
				row.LaunchedFromSessionID = &parent
				return row
			}(),
			wantPending: true,
		},
		{
			name:        "missing parent",
			child:       notifyRow("pi", true, "missing", false),
			wantPending: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newParentNotifyTestRouter(t)
			for _, seed := range tc.seed {
				r.handleOutcome(upsertOutcome(seed.id, seed.row))
			}
			r.handleOutcome(upsertOutcome("child", tc.child))
			closed := tc.child
			closed.Active = false
			r.handleOutcome(upsertOutcome("child", closed))
			if got := hasPendingNotification(r, "child"); got != tc.wantPending {
				t.Fatalf("pending child notification = %v, want %v", got, tc.wantPending)
			}
		})
	}
}

func TestCentralNotifyReparentChangesSuppressor(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("old", notifyRow("pi", true, "", false)))
	r.handleOutcome(upsertOutcome("new", notifyRow("pi", false, "", false)))
	child := notifyRow("pi", true, "old", false)
	r.handleOutcome(upsertOutcome("child", child))
	newParent := centralstore.SessionID("new")
	child.ParentSessionID = &newParent
	r.handleOutcome(upsertOutcome("child", child))
	child.Active = false
	r.handleOutcome(upsertOutcome("child", child))
	if !hasPendingNotification(r, "child") {
		t.Fatal("completion must follow the inactive current parent after reparenting")
	}
}

func TestCentralNotifyInactiveParentChangeReconsidersSuppression(t *testing.T) {
	tests := []struct {
		name       string
		newParent  string
		parentLive bool
		want       bool
	}{
		{name: "promotion", want: true},
		{name: "inactive parent", newParent: "new", want: true},
		{name: "active parent", newParent: "new", parentLive: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newParentNotifyTestRouter(t)
			r.handleOutcome(upsertOutcome("old", notifyRow("pi", true, "", false)))
			if tc.newParent != "" {
				r.handleOutcome(upsertOutcome(tc.newParent, notifyRow("pi", tc.parentLive, "", false)))
			}
			child := notifyRow("pi", true, "old", false)
			r.handleOutcome(upsertOutcome("child", child))
			child.Active = false
			r.handleOutcome(upsertOutcome("child", child))
			if hasPendingNotification(r, "child") {
				t.Fatal("completion escaped initial suppression")
			}

			if tc.newParent == "" {
				child.ParentSessionID = nil
			} else {
				parent := centralstore.SessionID(tc.newParent)
				child.ParentSessionID = &parent
			}
			// Unread may already have arrived while suppressed. Changing the edge
			// must release that withheld attention when the new parent permits it.
			child.Unread = true
			r.handleOutcome(upsertOutcome("child", child))
			if got := hasPendingNotification(r, "child"); got != tc.want {
				t.Fatalf("pending notification = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCentralNotifyParentChangeDoesNotRedeliverUnreadToken(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("new-parent", notifyRow("pi", false, "", false)))
	child := notifyRow("pi", true, "", false)
	r.handleOutcome(upsertOutcome("child", child))
	child.Active = false
	child.Unread = true
	child.UnreadToken = "result-1"
	r.handleOutcome(upsertOutcome("child", child))
	if !hasPendingNotification(r, "child") {
		t.Fatal("initial unread notification was not scheduled")
	}
	r.firePending("child")
	if hasPendingNotification(r, "child") {
		t.Fatal("delivered unread remained pending")
	}

	parent := centralstore.SessionID("new-parent")
	child.ParentSessionID = &parent
	r.handleOutcome(upsertOutcome("child", child))
	if hasPendingNotification(r, "child") {
		t.Fatal("parent change redelivered an already delivered unread token")
	}
}

func TestCentralNotifyFusedCompletionUnreadUsesParentSuppression(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
	child := notifyRow("pi", true, "parent", false)
	r.handleOutcome(upsertOutcome("child", child))

	// Runner status edges commit inactive + unread + token atomically. The
	// unread half must not schedule before suppression for the same edge.
	child.Active = false
	child.Unread = true
	child.UnreadToken = "result-1"
	r.handleOutcome(upsertOutcome("child", child))
	if hasPendingNotification(r, "child") {
		t.Fatal("fused completion unread escaped active direct-parent suppression")
	}
}

func TestCentralNotifyProcessChildUsesAgentParentSuppression(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", true, "parent", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", false, "parent", false)))
	if hasPendingNotification(r, "shell-child") {
		t.Fatal("a process child must inherit its active agent parent's notification suppression")
	}
}

func TestCentralNotifyActiveProcessParentSuppressesDirectChild(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("shell-parent", notifyRow("shell", true, "", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", true, "shell-parent", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", false, "shell-parent", false)))
	if hasPendingNotification(r, "shell-child") {
		t.Fatal("direct process activity is a launcher boundary too")
	}
}

func TestCentralNotifyDeadParentIsInactiveEvenWithStaleActiveFact(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	parent := notifyRow("shell", true, "", false)
	r.handleOutcome(upsertOutcome("parent", parent))
	parent.ID, parent.StatusReported = "parent", true
	r.handleOutcome(sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: "parent", Session: &parent, Alive: false})
	r.handleOutcome(upsertOutcome("child", notifyRow("shell", true, "parent", false)))
	r.handleOutcome(upsertOutcome("child", notifyRow("shell", false, "parent", false)))
	if !hasPendingNotification(r, "child") {
		t.Fatal("a dead direct parent cannot suppress child crash/completion delivery")
	}
}

func TestCentralNotifyOSCCommandCrashWithDeadParentDeliversUnread(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	parent := notifyRow("shell", true, "", false)
	parent.ID, parent.StatusReported = "parent", true
	r.handleOutcome(sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: "parent", Session: &parent, Alive: false})
	child := notifyRow("shell", true, "parent", false)
	r.handleOutcome(upsertOutcome("child", child))
	child.ID, child.StatusReported, child.Unread = "child", true, true
	r.handleOutcome(sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: "child", Session: &child, Alive: false})
	if !hasPendingNotification(r, "child") {
		t.Fatal("OSC C → crash before D with a dead parent must deliver unread attention")
	}
}

func TestCentralNotifyFocusedVisibleSessionKeepsUnreadWithoutDelivery(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.presence.Add(&presence.Client{ID: "viewer", Visibility: "visible", Focused: true, SelectedSessionID: "child"})
	row := notifyRow("pi", false, "", false)
	r.handleOutcome(upsertOutcome("child", row))
	row.Unread = true
	r.handleOutcome(upsertOutcome("child", row))
	if hasPendingNotification(r, "child") {
		t.Fatal("focused visible session must not receive completion delivery")
	}
	r.mu.Lock()
	unread := r.prevState["child"].Unread
	r.mu.Unlock()
	if !unread {
		t.Fatal("focused viewing suppresses delivery, not unread")
	}
}

func TestCentralNotifyHiddenOrUnfocusedSelectionDoesNotSuppress(t *testing.T) {
	for _, client := range []*presence.Client{
		{ID: "hidden", Visibility: "hidden", Focused: true, SelectedSessionID: "child"},
		{ID: "unfocused", Visibility: "visible", Focused: false, SelectedSessionID: "child"},
	} {
		t.Run(client.ID, func(t *testing.T) {
			r := newParentNotifyTestRouter(t)
			r.presence.Add(client)
			row := notifyRow("pi", false, "", false)
			r.handleOutcome(upsertOutcome("child", row))
			row.Unread = true
			r.handleOutcome(upsertOutcome("child", row))
			if !hasPendingNotification(r, "child") {
				t.Fatal("selection without focused visibility suppressed delivery")
			}
		})
	}
}

func TestCentralReadRouteWaitsForCommitToInstallFenceBeforeCancellation(t *testing.T) {
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fencedStore := &readCommitFenceStore{Store: store, committed: make(chan struct{}), release: make(chan struct{})}

	id := centralstore.SessionID("1eadfenc")
	token := "result-1"
	incarnation := "replacement-incarnation"
	endpoint := filepath.Join(t.TempDir(), "replacement.sock")
	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	cleared := make(chan struct{})
	allowResponse := make(chan struct{})
	var unread atomic.Bool
	unread.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /read", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Gmux-Expect-Incarnation") != incarnation || req.URL.Query().Get("token") != token {
			http.Error(w, "wrong acknowledgement owner", http.StatusConflict)
			return
		}
		unread.Store(false)
		close(cleared)
		<-allowResponse
		w.WriteHeader(http.StatusNoContent)
	})
	runnerServer := &http.Server{Handler: mux}
	go func() { _ = runnerServer.Serve(ln) }()
	t.Cleanup(func() { _ = runnerServer.Close(); _ = ln.Close() })

	fleet := newHarnessFleet(0)
	unreadFact := true
	fleet.metas[endpoint] = sessioncoord.RunnerMeta{
		Incarnation: incarnation,
		Registration: centralstore.RunnerRegistration{
			ID: id, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1,
			Facts: centralstore.RunnerFacts{Unread: &unreadFact, UnreadToken: &token},
		},
	}
	fleet.streams[endpoint] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: incarnation}
	coord := sessioncoord.New(nil, fleet, fencedStore, nil, nil)
	defer coord.Close()
	boot := &Bootstrap{Store: store, Coordinator: coord, Registry: coord.Registry()}

	registerDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: endpoint, AssertedID: id})
		registerDone <- err
	}()
	<-fencedStore.committed // durable commit landed; live owner is not installed

	router := newParentNotifyTestRouter(t)
	router.scheduleNotification(string(id), "unread", "Child", "New output", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+string(id)+"/read?token="+token, nil)
	originalOwnerResolution := acknowledgementRuntime
	ownerResolutionStarted := make(chan struct{})
	var ownerResolutionOnce sync.Once
	acknowledgementRuntime = func(c *sessioncoord.Coordinator, target centralstore.SessionID) (sessioncoord.Runtime, bool) {
		ownerResolutionOnce.Do(func() { close(ownerResolutionStarted) })
		return originalOwnerResolution(c, target)
	}
	t.Cleanup(func() { acknowledgementRuntime = originalOwnerResolution })

	routeDone := make(chan struct{})
	go func() {
		handleCentralSessionAction(rec, req, boot, newSSEFanout(), nil, nil, nil, "", router)
		close(routeDone)
	}()
	<-ownerResolutionStarted // route is now resolving while c.mu is fence-held
	select {
	case <-cleared:
		t.Fatal("read reached a runner inside commit-to-install")
	default:
	}
	if !hasPendingNotification(router, string(id)) {
		t.Fatal("delivery canceled before any owner acknowledged the result")
	}

	close(fencedStore.release) // install the committed live generation
	if err := <-registerDone; err != nil {
		t.Fatal(err)
	}
	<-cleared
	if unread.Load() {
		t.Fatal("installed live owner did not clear the valid token")
	}
	if !hasPendingNotification(router, string(id)) {
		t.Fatal("delivery canceled before the live acknowledgement completed")
	}
	close(allowResponse)
	<-routeDone
	if rec.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", rec.Code, rec.Body.String())
	}
	if hasPendingNotification(router, string(id)) {
		t.Fatal("successful installed-owner acknowledgement did not cancel delivery")
	}
}

func TestCentralReadRouteCancelsFocusedInteractionAttention(t *testing.T) {
	store, boot := openHarness(t, t.TempDir(), newHarnessFleet(0), nil)
	defer store.Close()
	exited := centralstore.UnixMillis(10)
	row, _, err := store.InsertSession(context.Background(), centralstore.NewSession{
		ID: "child", Adapter: "shell", Command: []string{"sh"}, Remotes: map[string]string{},
		Unread: true, CreatedAt: 1, ExitedAt: &exited,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newParentNotifyTestRouter(t)
	r.scheduleNotification(string(row.ID), "unread", "Child", "New output", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/child/read?token=", nil)
	handleCentralSessionAction(rec, req, boot, newSSEFanout(), nil, nil, nil, "", r)
	if rec.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", rec.Code, rec.Body.String())
	}
	if hasPendingNotification(r, "child") {
		t.Fatal("successful /read did not cancel pending attention")
	}
}

func TestCentralReadRouteRejectsDelayedAcknowledgementForNewerCompletion(t *testing.T) {
	store, boot := openHarness(t, t.TempDir(), newHarnessFleet(0), nil)
	defer store.Close()
	exited := centralstore.UnixMillis(10)
	row, _, err := store.InsertSession(context.Background(), centralstore.NewSession{
		ID: "child", Adapter: "shell", Command: []string{"sh"}, Remotes: map[string]string{},
		Unread: true, UnreadToken: "turn-2", CreatedAt: 1, ExitedAt: &exited,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := newParentNotifyTestRouter(t)
	router.scheduleNotification(string(row.ID), "unread", "Child", "New output", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/child/read?token=turn-1", nil)
	handleCentralSessionAction(rec, req, boot, newSSEFanout(), nil, nil, nil, "", router)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"result_changed"`) {
		t.Fatalf("read status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, ok, err := store.Session(context.Background(), row.ID)
	if err != nil || !ok || !got.Unread || got.UnreadToken != "turn-2" {
		t.Fatalf("newer completion changed: %+v ok=%v err=%v", got, ok, err)
	}
	if !hasPendingNotification(router, "child") {
		t.Fatal("rejected old acknowledgement canceled newer attention")
	}
}

func TestCentralNotifyUnreadConsumptionCancelsPending(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	row := notifyRow("pi", false, "", false)
	r.handleOutcome(upsertOutcome("child", row))
	row.Unread = true
	r.handleOutcome(upsertOutcome("child", row))
	if !hasPendingNotification(r, "child") {
		t.Fatal("test setup did not schedule unread attention")
	}
	row.Unread = false
	r.handleOutcome(upsertOutcome("child", row))
	if hasPendingNotification(r, "child") {
		t.Fatal("consumption must cancel pending attention")
	}
}

func TestCentralNotifyCancellationLinearizesWithPendingDelivery(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.presence.Add(&presence.Client{ID: "viewer", NotificationPermission: "granted"})
	r.scheduleNotification("child", "finished", "Child", "Task finished", "")

	dequeued := make(chan struct{})
	releaseDelivery := make(chan struct{})
	cancelAttempted := make(chan struct{})
	r.afterPendingDequeue = func() {
		close(dequeued)
		<-releaseDelivery
	}
	r.beforeCancelLock = func() { close(cancelAttempted) }
	fireDone := make(chan struct{})
	go func() {
		r.firePending("child")
		close(fireDone)
	}()
	<-dequeued // timer has deleted pending but has not inserted/sent active
	cancelDone := make(chan struct{})
	go func() {
		r.CancelForSession("child")
		close(cancelDone)
	}()
	<-cancelAttempted // cancellation is now contending with that exact gap
	close(releaseDelivery)
	select {
	case <-fireDone:
	case <-time.After(time.Second):
		t.Fatal("pending delivery did not finish")
	}
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not finish")
	}
	r.mu.Lock()
	_, pending := r.pending["child"]
	active := len(r.active)
	r.mu.Unlock()
	if pending || active != 0 {
		t.Fatalf("attention survived serialized cancellation: pending=%v active=%d", pending, active)
	}
}

func TestCentralNotifySuppressionCancelsExistingChildAttention(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
	child := notifyRow("pi", true, "parent", false)
	r.handleOutcome(upsertOutcome("child", child))
	withUnread := child
	withUnread.Unread = true
	r.handleOutcome(upsertOutcome("child", withUnread))
	if !hasPendingNotification(r, "child") {
		t.Fatal("test setup did not schedule child unread attention")
	}
	closed := withUnread
	closed.Active = false
	r.handleOutcome(upsertOutcome("child", closed))
	if hasPendingNotification(r, "child") {
		t.Fatal("managed completion must cancel existing child attention")
	}
}

func TestCentralNotifySuppressionSurvivesLateUnreadOutcome(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
	child := notifyRow("pi", true, "parent", false)
	r.handleOutcome(upsertOutcome("child", child))
	closed := child
	closed.Active = false
	r.handleOutcome(upsertOutcome("child", closed))
	lateUnread := closed
	lateUnread.Unread = true
	r.handleOutcome(upsertOutcome("child", lateUnread))
	if hasPendingNotification(r, "child") {
		t.Fatal("late unread fact resurrected permanently suppressed child attention")
	}
}

func TestCentralNotifySuppressionUsesParentStateAtChildCommit(t *testing.T) {
	t.Run("parent closes before child", func(t *testing.T) {
		r := newParentNotifyTestRouter(t)
		r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
		r.handleOutcome(upsertOutcome("child", notifyRow("pi", true, "parent", false)))
		r.handleOutcome(upsertOutcome("parent", notifyRow("pi", false, "", false)))
		r.handleOutcome(upsertOutcome("child", notifyRow("pi", false, "parent", false)))
		if !hasPendingNotification(r, "child") {
			t.Fatal("child committed after parent became inactive must notify")
		}
	})

	t.Run("parent closes after child", func(t *testing.T) {
		r := newParentNotifyTestRouter(t)
		r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
		r.handleOutcome(upsertOutcome("child", notifyRow("pi", true, "parent", false)))
		r.handleOutcome(upsertOutcome("child", notifyRow("pi", false, "parent", false)))
		r.handleOutcome(upsertOutcome("parent", notifyRow("pi", false, "", false)))
		if hasPendingNotification(r, "child") {
			t.Fatal("later parent inactivity must not retroactively notify the child")
		}
	})
}
