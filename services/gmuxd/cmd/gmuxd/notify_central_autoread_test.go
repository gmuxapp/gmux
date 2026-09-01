package main

import (
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func suppressedOutcome(token string) sessioncoord.Outcome {
	row := notifyRow("shell", false, "parent", false)
	row.UnreadToken = token
	at, code := centralstore.UnixMillis(20), 0
	row.ExitedAt, row.ExitCode = &at, &code
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: "child", Session: &row, AttentionSuppressed: true}
}

func TestCentralNotifyCommittedAutoReadCancelsOnlyExactResult(t *testing.T) {
	t.Run("pending exact generation", func(t *testing.T) {
		r := newParentNotifyTestRouter(t)
		r.scheduleNotification("child", "unread", "Child", "New output", "shell", "result-2")
		r.handleOutcome(suppressedOutcome("result-2"))
		if hasPendingNotification(r, "child") {
			t.Fatal("committed suppression did not cancel its pending result")
		}
	})
	t.Run("empty token is unscoped and canceled", func(t *testing.T) {
		r := newParentNotifyTestRouter(t)
		r.scheduleNotification("child", "finished", "Child", "Finished", "shell")
		r.active["notif-unscoped"] = activeCentralNotif{sessionID: "child"}
		r.handleOutcome(suppressedOutcome("result-2"))
		if hasPendingNotification(r, "child") {
			t.Fatal("committed suppression did not cancel unscoped pending attention")
		}
		if _, ok := r.active["notif-unscoped"]; ok {
			t.Fatal("committed suppression did not cancel unscoped active attention")
		}
	})
	t.Run("mismatched non-empty older generation survives", func(t *testing.T) {
		r := newParentNotifyTestRouter(t)
		r.scheduleNotification("child", "unread", "Child", "New output", "shell", "result-1")
		r.handleOutcome(suppressedOutcome("result-2"))
		if !hasPendingNotification(r, "child") {
			t.Fatal("suppression canceled older result attention")
		}
	})
	t.Run("active exact generation", func(t *testing.T) {
		r := newParentNotifyTestRouter(t)
		r.active["notif-exact"] = activeCentralNotif{sessionID: "child", resultToken: "result-2"}
		r.active["notif-old"] = activeCentralNotif{sessionID: "child", resultToken: "result-1"}
		r.handleOutcome(suppressedOutcome("result-2"))
		if _, ok := r.active["notif-exact"]; ok {
			t.Fatal("committed suppression did not cancel its active result")
		}
		if _, ok := r.active["notif-old"]; !ok {
			t.Fatal("committed suppression canceled older active result")
		}
	})
}

func TestCentralNotifyFailureStillProducesAttention(t *testing.T) {
	r := newParentNotifyTestRouter(t)
	row := notifyRow("shell", false, "parent", false)
	row.Unread, row.UnreadToken = true, "failed-result"
	r.handleOutcome(upsertOutcome("child", notifyRow("shell", true, "parent", false)))
	r.handleOutcome(upsertOutcome("child", row))
	if !hasPendingNotification(r, "child") {
		t.Fatal("failed exit lost strict attention")
	}
}
