package presence

import (
	"testing"
	"time"
)

// nilClient creates a Client with no WebSocket connection (sufficient for table logic tests).
func nilClient(id, deviceType, perm string, lastInteraction float64) *Client {
	return &Client{
		ID:                     id,
		DeviceType:             deviceType,
		NotificationPermission: perm,
		LastInteraction:        lastInteraction,
		ConnectedAt:            time.Now(),
	}
}

func nowSecs() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

func TestAnyFocused(t *testing.T) {
	tbl := New(Callbacks{})
	if tbl.AnyFocused() {
		t.Fatal("empty table should not have focused client")
	}

	c := nilClient("a", "desktop", "granted", nowSecs())
	tbl.Add(c)
	if tbl.AnyFocused() {
		t.Fatal("unfocused client should not count")
	}

	tbl.Update("a", ClientState{Focused: true})
	if !tbl.AnyFocused() {
		t.Fatal("focused client should be detected")
	}

	tbl.Update("a", ClientState{Focused: false})
	if tbl.AnyFocused() {
		t.Fatal("unfocused client should no longer count")
	}
}

func TestAnyViewing(t *testing.T) {
	tbl := New(Callbacks{})
	c := nilClient("a", "desktop", "granted", nowSecs())
	tbl.Add(c)

	tbl.Update("a", ClientState{Focused: true, SelectedSessionID: "sess-1"})

	if !tbl.AnyViewing("sess-1") {
		t.Fatal("should detect client viewing sess-1")
	}
	if tbl.AnyViewing("sess-2") {
		t.Fatal("should not detect client viewing sess-2")
	}

	// Unfocused client viewing a session should not count.
	tbl.Update("a", ClientState{Focused: false, SelectedSessionID: "sess-1"})
	if tbl.AnyViewing("sess-1") {
		t.Fatal("unfocused client should not count as viewing")
	}
}

func TestBestNotifyTarget_NoGranted(t *testing.T) {
	tbl := New(Callbacks{})
	tbl.Add(nilClient("a", "desktop", "default", nowSecs()))
	tbl.Add(nilClient("b", "mobile", "denied", nowSecs()))

	if tbl.BestNotifyTarget(2*time.Minute) != nil {
		t.Fatal("should return nil when no client has granted permission")
	}
}

func TestBestNotifyTarget_SingleClient(t *testing.T) {
	tbl := New(Callbacks{})
	c := nilClient("a", "desktop", "granted", nowSecs())
	tbl.Add(c)

	target := tbl.BestNotifyTarget(2 * time.Minute)
	if target == nil || target.ID != "a" {
		t.Fatal("should return the single granted client")
	}
}

func TestBestNotifyTarget_MostRecentInteraction(t *testing.T) {
	tbl := New(Callbacks{})
	now := nowSecs()
	tbl.Add(nilClient("old", "desktop", "granted", now-10))
	tbl.Add(nilClient("new", "desktop", "granted", now))

	target := tbl.BestNotifyTarget(2 * time.Minute)
	if target == nil || target.ID != "new" {
		t.Fatalf("should prefer most recently interacted client, got %v", target)
	}
}

func TestBestNotifyTarget_CrossDeviceWhenIdle(t *testing.T) {
	tbl := New(Callbacks{})
	now := nowSecs()
	// Desktop was used 5 minutes ago (idle), mobile 30 seconds ago.
	tbl.Add(nilClient("desktop", "desktop", "granted", now-300))
	tbl.Add(nilClient("mobile", "mobile", "granted", now-30))

	// With a 2-minute idle threshold, desktop is idle.
	// Should prefer mobile since it's a different device type.
	target := tbl.BestNotifyTarget(2 * time.Minute)
	if target == nil || target.ID != "mobile" {
		t.Fatalf("should route to mobile when desktop is idle, got %v", target)
	}
}

func TestBestNotifyTarget_CrossDeviceNotTriggeredWhenActive(t *testing.T) {
	tbl := New(Callbacks{})
	now := nowSecs()
	// Desktop used 30 seconds ago (not idle), mobile 60 seconds ago.
	tbl.Add(nilClient("desktop", "desktop", "granted", now-30))
	tbl.Add(nilClient("mobile", "mobile", "granted", now-60))

	target := tbl.BestNotifyTarget(2 * time.Minute)
	if target == nil || target.ID != "desktop" {
		t.Fatalf("should keep desktop when it's not idle, got %v", target)
	}
}

func TestBestNotifyTarget_AllIdle_NoCrossDevice(t *testing.T) {
	tbl := New(Callbacks{})
	now := nowSecs()
	// Both desktop clients are idle, no cross-device candidate.
	tbl.Add(nilClient("d1", "desktop", "granted", now-300))
	tbl.Add(nilClient("d2", "desktop", "granted", now-200))

	target := tbl.BestNotifyTarget(2 * time.Minute)
	if target == nil || target.ID != "d2" {
		t.Fatalf("should pick most recent same-type when no cross-device available, got %v", target)
	}
}

func TestCallbacks_OnClientFocused(t *testing.T) {
	var focusedID string
	tbl := New(Callbacks{
		OnClientFocused: func(id string) { focusedID = id },
	})
	tbl.Add(nilClient("a", "desktop", "granted", nowSecs()))

	// First focus should fire.
	tbl.Update("a", ClientState{Focused: true})
	if focusedID != "a" {
		t.Fatal("OnClientFocused should have fired")
	}

	// Already focused → should not fire again.
	focusedID = ""
	tbl.Update("a", ClientState{Focused: true})
	if focusedID != "" {
		t.Fatal("OnClientFocused should not fire when already focused")
	}

	// Unfocus then refocus.
	tbl.Update("a", ClientState{Focused: false})
	tbl.Update("a", ClientState{Focused: true})
	if focusedID != "a" {
		t.Fatal("OnClientFocused should fire on re-focus")
	}
}

func TestCallbacks_OnSessionSelected(t *testing.T) {
	var selectedSession string
	tbl := New(Callbacks{
		OnSessionSelected: func(_, sessID string) { selectedSession = sessID },
	})
	tbl.Add(nilClient("a", "desktop", "granted", nowSecs()))

	tbl.Update("a", ClientState{SelectedSessionID: "s1"})
	if selectedSession != "s1" {
		t.Fatal("OnSessionSelected should fire on session change")
	}

	// Same session again → should not fire.
	selectedSession = ""
	tbl.Update("a", ClientState{SelectedSessionID: "s1"})
	if selectedSession != "" {
		t.Fatal("OnSessionSelected should not fire when session unchanged")
	}

	// Empty session → should not fire.
	selectedSession = ""
	tbl.Update("a", ClientState{SelectedSessionID: ""})
	if selectedSession != "" {
		t.Fatal("OnSessionSelected should not fire for empty session")
	}

	// New session.
	tbl.Update("a", ClientState{SelectedSessionID: "s2"})
	if selectedSession != "s2" {
		t.Fatal("OnSessionSelected should fire for new session")
	}
}

func TestRemove(t *testing.T) {
	tbl := New(Callbacks{})
	c := nilClient("a", "desktop", "granted", nowSecs())
	tbl.Add(c)
	tbl.Update("a", ClientState{Focused: true})

	tbl.Remove("a")
	if tbl.AnyFocused() {
		t.Fatal("removed client should not be counted")
	}
}

// TestOnClientConnectedFires verifies the callback fires exactly once
// per Add, carrying the client's ID. This is the hook peering uses to
// nudge dial-out-only peers to reconnect when a user shows up.
func TestOnClientConnectedFires(t *testing.T) {
	var gotIDs []string
	tbl := New(Callbacks{
		OnClientConnected: func(id string) { gotIDs = append(gotIDs, id) },
	})

	tbl.Add(nilClient("a", "desktop", "granted", nowSecs()))
	tbl.Add(nilClient("b", "mobile", "granted", nowSecs()))

	if len(gotIDs) != 2 || gotIDs[0] != "a" || gotIDs[1] != "b" {
		t.Errorf("want callback fired for [a b], got %v", gotIDs)
	}
}

// TestOnClientConnectedNilSafe verifies Add works when no callback is set.
func TestOnClientConnectedNilSafe(t *testing.T) {
	tbl := New(Callbacks{})
	tbl.Add(nilClient("a", "desktop", "granted", nowSecs())) // must not panic
	if got := len(tbl.Conns()); got != 1 {
		t.Errorf("client should be registered, got %d conns", got)
	}
}
