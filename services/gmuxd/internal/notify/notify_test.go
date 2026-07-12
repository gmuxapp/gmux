package notify

import (
	"context"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// testConfig uses short durations for fast tests.
func testConfig() Config {
	return Config{
		GracePeriod:   50 * time.Millisecond,
		IdleThreshold: 2 * time.Minute,
	}
}

func nowSecs() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

// testEnv bundles a store, presence table, and router for testing.
type testEnv struct {
	store    *store.Store
	presence *presence.Table
	router   *Router
	cancel   context.CancelFunc
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	s := store.New()
	router := (*Router)(nil)
	p := presence.New(presence.Callbacks{
		OnClientFocused: func(_ string) {
			if router != nil {
				router.CancelAllPending()
			}
		},
		OnSessionSelected: func(_, sessID string) {
			if router != nil {
				router.CancelForSession(sessID)
			}
		},
	})
	router = New(p, s, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	go router.Run(ctx)

	t.Cleanup(func() { cancel() })
	return &testEnv{store: s, presence: p, router: router, cancel: cancel}
}

// addClient adds a focused, granted client to the presence table with a nil conn.
// The nil conn means sendJSON will log an error but not crash — sufficient for
// testing routing decisions.
func (e *testEnv) addClient(id, deviceType string) {
	e.presence.Add(&presence.Client{
		ID:                     id,
		DeviceType:             deviceType,
		NotificationPermission: "granted",
		LastInteraction:        nowSecs(),
		ConnectedAt:            time.Now(),
	})
}

// upsertSession creates or updates a session in the store.
func (e *testEnv) upsertSession(id string, working, unread, alive bool) {
	var status *store.Status
	if working {
		status = &store.Status{Working: true}
	} else {
		status = &store.Status{Working: false}
	}
	e.store.Upsert(store.Session{
		ID:        id,
		Title:     "test-" + id,
		Alive:     alive,
		Status:    status,
		Unread:    unread,
		StartedAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	})
}

func TestTransition_WorkingToIdle_SchedulesNotification(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	// Client is not focused → notifications should fire.
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	// Seed session as working.
	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond) // let router process

	// Transition to idle.
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if !hasPending {
		t.Fatal("expected a pending notification for s1 after working→idle transition")
	}
}

func TestTransition_UnreadFlip_SchedulesNotification(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	env.upsertSession("s1", false, true, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if !hasPending {
		t.Fatal("expected a pending notification for s1 after unread flip")
	}
}

func TestNoNotification_WhenFocused(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         true,
		LastInteraction: nowSecs(),
	})

	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if hasPending {
		t.Fatal("should not schedule notification when a client is focused")
	}
}

func TestNoNotification_WhenViewing(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:           true,
		SelectedSessionID: "s1",
		LastInteraction:   nowSecs(),
	})

	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if hasPending {
		t.Fatal("should not schedule notification when client is viewing the session")
	}
}

func TestNewSession_NoTransition(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	// First time seeing this session — already idle. Should not fire.
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if hasPending {
		t.Fatal("should not fire for a new session that starts idle")
	}
}

func TestCancelAllPending_OnFocus(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	// Verify pending exists.
	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()
	if !hasPending {
		t.Fatal("expected pending notification before focus")
	}

	// User focuses gmux → should cancel all pending.
	env.presence.Update("c1", presence.ClientState{
		Focused:         true,
		LastInteraction: nowSecs(),
	})

	env.router.mu.Lock()
	_, stillPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if stillPending {
		t.Fatal("pending notification should have been cancelled on focus")
	}
}

func TestCancelForSession_OnSelect(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	// User selects s1 → should cancel pending for s1.
	env.presence.Update("c1", presence.ClientState{
		Focused:           false,
		SelectedSessionID: "s1",
		LastInteraction:   nowSecs(),
	})

	env.router.mu.Lock()
	_, stillPending := env.router.pending["s1"]
	env.router.mu.Unlock()

	if stillPending {
		t.Fatal("pending notification should have been cancelled when session selected")
	}
}

func TestGracePeriod_FiresAfterDelay(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)
	env.upsertSession("s1", false, false, true)
	time.Sleep(20 * time.Millisecond)

	// Should be pending now, not yet fired.
	env.router.mu.Lock()
	_, hasPending := env.router.pending["s1"]
	env.router.mu.Unlock()
	if !hasPending {
		t.Fatal("expected pending notification")
	}

	// Wait for grace period to expire (50ms + margin).
	time.Sleep(80 * time.Millisecond)

	// Should have been removed from pending (fired or dropped).
	env.router.mu.Lock()
	_, stillPending := env.router.pending["s1"]
	env.router.mu.Unlock()
	if stillPending {
		t.Fatal("notification should have fired after grace period")
	}
}

func TestFinishedPreferredOverUnread(t *testing.T) {
	env := newTestEnv(t)
	env.addClient("c1", "desktop")
	env.presence.Update("c1", presence.ClientState{
		Focused:         false,
		LastInteraction: nowSecs(),
	})

	// Start working.
	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)

	// Unread arrives first.
	env.upsertSession("s1", true, true, true)
	time.Sleep(20 * time.Millisecond)

	// Then finishes.
	env.upsertSession("s1", false, true, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	p, ok := env.router.pending["s1"]
	notifType := ""
	if ok {
		notifType = p.notifType
	}
	env.router.mu.Unlock()

	if !ok {
		t.Fatal("expected pending notification")
	}
	if notifType != "finished" {
		t.Fatalf("expected 'finished' to override 'unread', got %q", notifType)
	}
}

func TestSessionRemove_CleansUpPrevState(t *testing.T) {
	env := newTestEnv(t)

	env.upsertSession("s1", true, false, true)
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, exists := env.router.prevState["s1"]
	env.router.mu.Unlock()
	if !exists {
		t.Fatal("prevState should contain s1")
	}

	env.store.Remove("s1")
	time.Sleep(20 * time.Millisecond)

	env.router.mu.Lock()
	_, exists = env.router.prevState["s1"]
	env.router.mu.Unlock()
	if exists {
		t.Fatal("prevState should be cleaned up after session-remove")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m 30s"},
		{5 * time.Minute, "5m 0s"},
		{65 * time.Minute, "1h 5m"},
		{2*time.Hour + 30*time.Minute, "2h 30m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
