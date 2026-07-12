// Package presence tracks connected browser clients and their visibility,
// focus, and interaction state. The notification router uses this to decide
// whether and where to deliver notifications.
package presence

import (
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Client represents a single connected browser tab.
type Client struct {
	ID                     string
	Conn                   *websocket.Conn
	DeviceType             string // "desktop" | "mobile"
	NotificationPermission string // "granted" | "denied" | "default" | "unavailable"
	Visibility             string // "visible" | "hidden"
	Focused                bool
	SelectedSessionID      string
	LastInteraction        float64 // Unix timestamp (seconds)
	ConnectedAt            time.Time
}

// Callbacks lets the notification router react to presence changes
// synchronously inside Update(). Set these before any clients connect.
type Callbacks struct {
	// OnClientConnected fires after a new browser client is registered.
	// It's the "a user is here and wants fresh data" signal — used to
	// nudge dial-out-only peer connections to retry immediately rather
	// than wait out a backoff.
	OnClientConnected func(clientID string)
	OnClientFocused   func(clientID string)
	OnSessionSelected func(clientID string, sessionID string)
}

// Table is a concurrency-safe registry of connected browser clients.
type Table struct {
	mu        sync.RWMutex
	clients   map[string]*Client
	callbacks Callbacks
}

// New creates an empty presence table.
func New(cb Callbacks) *Table {
	return &Table{
		clients:   make(map[string]*Client),
		callbacks: cb,
	}
}

// Add registers a new client. Returns the assigned client ID.
func (t *Table) Add(client *Client) {
	t.mu.Lock()
	t.clients[client.ID] = client
	t.mu.Unlock()

	// Fire outside the lock to avoid holding it across the callback.
	if t.callbacks.OnClientConnected != nil {
		t.callbacks.OnClientConnected(client.ID)
	}
}

// Remove unregisters a client by ID.
func (t *Table) Remove(id string) {
	t.mu.Lock()
	delete(t.clients, id)
	t.mu.Unlock()
}

// ClientState is the mutable portion of a client's state, sent with each
// client-state message.
type ClientState struct {
	Visibility        string
	Focused           bool
	SelectedSessionID string
	LastInteraction   float64
}

// Update applies a state change to a client and fires relevant callbacks.
func (t *Table) Update(id string, state ClientState) {
	t.mu.Lock()
	c, ok := t.clients[id]
	if !ok {
		t.mu.Unlock()
		return
	}

	wasFocused := c.Focused
	prevSession := c.SelectedSessionID

	c.Visibility = state.Visibility
	c.Focused = state.Focused
	c.SelectedSessionID = state.SelectedSessionID
	c.LastInteraction = state.LastInteraction
	t.mu.Unlock()

	// Fire callbacks outside the lock to avoid deadlocks with the router.
	if !wasFocused && state.Focused && t.callbacks.OnClientFocused != nil {
		t.callbacks.OnClientFocused(id)
	}
	if state.SelectedSessionID != prevSession && state.SelectedSessionID != "" && t.callbacks.OnSessionSelected != nil {
		t.callbacks.OnSessionSelected(id, state.SelectedSessionID)
	}
}

// SetPermission updates a client's notification permission.
func (t *Table) SetPermission(id string, permission string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.clients[id]; ok {
		c.NotificationPermission = permission
	}
}

// AnyFocused returns true if any connected client has gmux focused.
func (t *Table) AnyFocused() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, c := range t.clients {
		if c.Focused {
			return true
		}
	}
	return false
}

// AnyViewing returns true if any client is focused and viewing the given session.
func (t *Table) AnyViewing(sessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, c := range t.clients {
		if c.Focused && c.SelectedSessionID == sessionID {
			return true
		}
	}
	return false
}

// Conns returns a snapshot of all client WebSocket connections.
// Safe to use for writes outside the lock.
func (t *Table) Conns() []*websocket.Conn {
	t.mu.RLock()
	defer t.mu.RUnlock()
	conns := make([]*websocket.Conn, 0, len(t.clients))
	for _, c := range t.clients {
		conns = append(conns, c.Conn)
	}
	return conns
}

// BestNotifyTarget returns the connected client most likely to reach the user,
// or nil if no client can show notifications.
//
// Selection criteria:
//  1. Must have notification_permission == "granted"
//  2. Prefer the client with the most recent last_interaction
//  3. If the most-recent client is idle (> idleThreshold since last interaction),
//     prefer a client on a different device type
func (t *Table) BestNotifyTarget(idleThreshold time.Duration) *Client {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var candidates []*Client
	for _, c := range t.clients {
		if c.NotificationPermission == "granted" {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Find most recently interacted client.
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.LastInteraction > best.LastInteraction {
			best = c
		}
	}

	// If the best (most recently used) client is idle, prefer a client on
	// a different device type — the user likely moved to that device. Among
	// cross-device candidates, pick the one with the most recent interaction.
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	idleSecs := idleThreshold.Seconds()
	if now-best.LastInteraction > idleSecs {
		var crossDevice *Client
		for _, c := range candidates {
			if c.DeviceType != best.DeviceType {
				if crossDevice == nil || c.LastInteraction > crossDevice.LastInteraction {
					crossDevice = c
				}
			}
		}
		if crossDevice != nil {
			best = crossDevice
		}
	}

	return best
}
