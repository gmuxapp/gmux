// Package notify implements the notification router. It subscribes to session
// store events, detects transitions (task finished, new output), applies a
// grace period and coalescing window, and delivers notifications to the best
// connected client via WebSocket.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"nhooyr.io/websocket"
)

// Config holds tunable parameters for the notification router.
type Config struct {
	GracePeriod   time.Duration // delay before firing (default 5s); also serves as the coalescing window
	IdleThreshold time.Duration // client idle threshold for cross-device routing (default 2m)
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		GracePeriod:   5 * time.Second,
		IdleThreshold: 2 * time.Minute,
	}
}

// NotifyMessage is sent to the browser over the presence WebSocket.
type NotifyMessage struct {
	Type      string `json:"type"` // "notify"
	ID        string `json:"id"`   // daemon-assigned notification ID
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Tag       string `json:"tag"`
}

// CancelMessage tells the browser to dismiss a notification.
type CancelMessage struct {
	Type string `json:"type"` // "cancel"
	ID   string `json:"id"`
}

type pendingNotif struct {
	sessionID string
	notifType string // "finished" | "unread"
	title     string
	body      string
	timer     *time.Timer
	notifID   string
}

// Router watches session state and delivers notifications to browser clients.
type Router struct {
	presence *presence.Table
	sessions *store.Store
	config   Config

	mu        sync.Mutex
	prevState map[string]sessionSnapshot
	pending   map[string]*pendingNotif // sessionID → pending
	active    map[string]activeNotif   // notifID → active (sent but not dismissed)
	nextID    int
}

type sessionSnapshot struct {
	Working bool
	Unread  bool
	Alive   bool
}

type activeNotif struct {
	sessionID string
	clientID  string
}

// New creates a notification router.
func New(p *presence.Table, s *store.Store, cfg Config) *Router {
	return &Router{
		presence:  p,
		sessions:  s,
		config:    cfg,
		prevState: make(map[string]sessionSnapshot),
		pending:   make(map[string]*pendingNotif),
		active:    make(map[string]activeNotif),
	}
}

func (r *Router) genID() string {
	r.nextID++
	return fmt.Sprintf("notif-%d", r.nextID)
}

// Run subscribes to store events and processes them until ctx is cancelled.
func (r *Router) Run(ctx context.Context) {
	ch, cancel := r.sessions.Subscribe()
	defer cancel()

	// Seed prevState from current sessions so we don't fire notifications
	// for pre-existing state on startup. Under r.mu like every other
	// prevState access: Run executes on its own goroutine, and presence
	// callbacks (CancelForSession et al.) can touch router state
	// concurrently from the moment the router is wired up.
	r.mu.Lock()
	for _, s := range r.sessions.List() {
		r.prevState[s.ID] = snapshotOf(s)
	}
	r.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			r.handleEvent(ev)
		}
	}
}

func snapshotOf(s store.Session) sessionSnapshot {
	working := false
	if s.Status != nil {
		working = s.Status.Working
	}
	return sessionSnapshot{
		Working: working,
		Unread:  s.Unread,
		Alive:   s.Alive,
	}
}

func (r *Router) handleEvent(ev store.Event) {
	if ev.Type != store.EventSessionUpsert || ev.Session == nil {
		// session-remove: clean up prevState
		if ev.Type == store.EventSessionRemove {
			r.mu.Lock()
			delete(r.prevState, ev.ID)
			r.mu.Unlock()
		}
		return
	}

	sess := *ev.Session
	cur := snapshotOf(sess)

	r.mu.Lock()
	prev, existed := r.prevState[sess.ID]
	r.prevState[sess.ID] = cur
	r.mu.Unlock()

	if !existed {
		return // new session, no transition to detect
	}

	// Transition: working → idle on a live session
	if prev.Working && !cur.Working && cur.Alive {
		body := formatFinishedBody(sess)
		r.scheduleNotification(sess.ID, "finished", sess.Title, body)
	}

	// Transition: unread flipped on
	if !prev.Unread && cur.Unread {
		r.scheduleNotification(sess.ID, "unread", sess.Title, "New output")
	}
}

func formatFinishedBody(sess store.Session) string {
	body := "Task finished"
	if sess.StartedAt != "" {
		if start, err := time.Parse(time.RFC3339Nano, sess.StartedAt); err == nil {
			dur := time.Since(start).Round(time.Second)
			body = fmt.Sprintf("Finished (%s)", formatDuration(dur))
		}
	}
	return body
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m = m % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

func (r *Router) scheduleNotification(sessionID, notifType, title, body string) {
	// Skip if user is viewing this session or focused on gmux.
	if r.presence.AnyViewing(sessionID) {
		return
	}
	if r.presence.AnyFocused() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// If there's already a pending notification for this session, update it
	// (prefer "finished" over "unread").
	if existing, ok := r.pending[sessionID]; ok {
		if notifType == "finished" && existing.notifType == "unread" {
			existing.notifType = notifType
			existing.title = title
			existing.body = body
		}
		return
	}

	notifID := r.genID()
	p := &pendingNotif{
		sessionID: sessionID,
		notifType: notifType,
		title:     title,
		body:      body,
		notifID:   notifID,
	}

	p.timer = time.AfterFunc(r.config.GracePeriod, func() {
		r.firePending(sessionID)
	})

	r.pending[sessionID] = p
}

func (r *Router) firePending(sessionID string) {
	// Extract the pending notification under the lock, then release before
	// calling into the presence table (avoids holding r.mu during RLock on
	// the presence table, which could slow down presence callbacks).
	r.mu.Lock()
	p, ok := r.pending[sessionID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.pending, sessionID)
	pendingCount := len(r.pending) + 1 // +1 for the one we just removed

	// Coalesce: if 3+ events are pending simultaneously, send a summary.
	if pendingCount >= 3 {
		count := 1
		for sid, other := range r.pending {
			other.timer.Stop()
			delete(r.pending, sid)
			count++
		}
		r.mu.Unlock()
		r.fireCoalesced(count)
		return
	}
	r.mu.Unlock()

	// Re-check: user may have focused gmux during the grace period.
	if r.presence.AnyFocused() || r.presence.AnyViewing(sessionID) {
		return
	}

	target := r.presence.BestNotifyTarget(r.config.IdleThreshold)
	if target == nil {
		log.Printf("notify: no target for session %s (no client with granted permission)", sessionID)
		return
	}

	msg := NotifyMessage{
		Type:      "notify",
		ID:        p.notifID,
		SessionID: sessionID,
		Title:     p.title,
		Body:      p.body,
		Tag:       sessionID,
	}

	r.mu.Lock()
	r.active[p.notifID] = activeNotif{sessionID: sessionID, clientID: target.ID}
	r.mu.Unlock()

	sendJSON(target.Conn, msg)
	log.Printf("notify: sent %s to client %s for session %s (%s)", p.notifID, target.ID, sessionID, p.notifType)

	// Auto-expire the active entry after 5 minutes so dismissed-without-click
	// notifications don't leak memory in the active map.
	time.AfterFunc(5*time.Minute, func() {
		r.mu.Lock()
		delete(r.active, p.notifID)
		r.mu.Unlock()
	})
}

func (r *Router) fireCoalesced(count int) {
	target := r.presence.BestNotifyTarget(r.config.IdleThreshold)
	if target == nil {
		log.Printf("notify: no target for coalesced notification (%d sessions)", count)
		return
	}

	notifID := func() string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.genID()
	}()

	msg := NotifyMessage{
		Type:  "notify",
		ID:    notifID,
		Title: "gmux",
		Body:  fmt.Sprintf("%d sessions finished", count),
		Tag:   "coalesced",
	}

	sendJSON(target.Conn, msg)
	log.Printf("notify: sent coalesced notification (%d sessions) to client %s", count, target.ID)
}

// CancelAllPending cancels all pending notifications (e.g. user focused gmux).
func (r *Router) CancelAllPending() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for sid, p := range r.pending {
		p.timer.Stop()
		delete(r.pending, sid)
	}
}

// CancelForSession cancels a pending or active notification for a session
// (e.g. user selected that session).
func (r *Router) CancelForSession(sessionID string) {
	r.mu.Lock()

	// Cancel pending
	if p, ok := r.pending[sessionID]; ok {
		p.timer.Stop()
		delete(r.pending, sessionID)
	}

	// Cancel active — find and remove, collect IDs to cancel
	var cancelIDs []string
	for nid, a := range r.active {
		if a.sessionID == sessionID {
			cancelIDs = append(cancelIDs, nid)
			delete(r.active, nid)
		}
	}
	r.mu.Unlock()

	for _, nid := range cancelIDs {
		r.broadcastCancel(nid)
	}
}

func (r *Router) broadcastCancel(notifID string) {
	msg := CancelMessage{Type: "cancel", ID: notifID}
	// Collect connections first, then write outside the presence lock
	// to avoid holding the lock during potentially slow WebSocket writes.
	for _, conn := range r.presence.Conns() {
		sendJSON(conn, msg)
	}
	log.Printf("notify: cancel %s", notifID)
}

func sendJSON(conn *websocket.Conn, v any) {
	if conn == nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		log.Printf("notify: write error: %v", err)
	}
}
