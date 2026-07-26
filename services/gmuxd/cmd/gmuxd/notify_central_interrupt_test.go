package main

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// upsertOutcome is the committed-row outcome the coordinator publishes.
func upsertOutcome(id string, row centralstore.Session) sessioncoord.Outcome {
	row.ID = centralstore.SessionID(id)
	row.StatusReported = true
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: &row, Alive: true}
}

// TestCentralNotifyInterruptionSuppressesFinished mirrors the legacy router's
// rule on the central path: an intentional stop closes the turn without a
// "finished" notification (ADR 0027), while an ordinary completion still
// notifies.
func TestCentralNotifyInterruptionSuppressesFinished(t *testing.T) {
	newRouter := func() *centralNotifyRouter {
		p := presence.New(presence.Callbacks{})
		return newCentralNotifyRouter(p, notifyConfig{GracePeriod: time.Hour, IdleThreshold: time.Minute})
	}

	t.Run("interrupted", func(t *testing.T) {
		r := newRouter()
		r.handleOutcome(upsertOutcome("s1", centralstore.Session{Active: true}))
		r.handleOutcome(upsertOutcome("s1", centralstore.Session{Interrupted: true}))
		r.mu.Lock()
		_, pending := r.pending["s1"]
		r.mu.Unlock()
		if pending {
			t.Fatal("interrupted turn must not schedule a finished notification")
		}
	})

	t.Run("completed still notifies", func(t *testing.T) {
		r := newRouter()
		r.handleOutcome(upsertOutcome("s1", centralstore.Session{Active: true}))
		r.handleOutcome(upsertOutcome("s1", centralstore.Session{}))
		r.mu.Lock()
		_, pending := r.pending["s1"]
		r.mu.Unlock()
		if !pending {
			t.Fatal("ordinary completion must still schedule a notification")
		}
	})

	t.Run("terminal error still notifies", func(t *testing.T) {
		r := newRouter()
		r.handleOutcome(upsertOutcome("s1", centralstore.Session{Active: true}))
		r.handleOutcome(upsertOutcome("s1", centralstore.Session{Error: true}))
		r.mu.Lock()
		_, pending := r.pending["s1"]
		r.mu.Unlock()
		if !pending {
			t.Fatal("a failed turn is not an intentional stop; it must still notify")
		}
	})
}
