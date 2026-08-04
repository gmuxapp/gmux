package main

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
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
	row := centralstore.Session{Adapter: adapterName, Active: active, PromotedToRoot: promoted}
	if parent != "" {
		id := centralstore.SessionID(parent)
		row.LaunchParentID = &id
	}
	return row
}

func hasPendingNotification(r *centralNotifyRouter, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pending[id]
	return ok
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
			name: "promoted child",
			seed: []struct {
				id  string
				row centralstore.Session
			}{{"parent", notifyRow("pi", true, "", false)}},
			child:       notifyRow("pi", true, "parent", true),
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

func TestCentralNotifyProcessChildUsesAgentParentSuppression(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("parent", notifyRow("pi", true, "", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", true, "parent", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", false, "parent", false)))
	if hasPendingNotification(r, "shell-child") {
		t.Fatal("a process child must inherit its active agent parent's notification suppression")
	}
}

func TestCentralNotifyNonAgentParentRemainsRootBoundary(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.handleOutcome(upsertOutcome("shell-parent", notifyRow("shell", true, "", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", true, "shell-parent", false)))
	r.handleOutcome(upsertOutcome("shell-child", notifyRow("shell", false, "shell-parent", false)))
	if !hasPendingNotification(r, "shell-child") {
		t.Fatal("a non-agent launch parent must not become a family management boundary")
	}
}

func TestCentralNotifyCancellationLinearizesWithPendingDelivery(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	r.presence.Add(&presence.Client{ID: "viewer", NotificationPermission: "granted"})
	r.scheduleNotification("child", "finished", "Child", "Task finished")

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
