package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"nhooyr.io/websocket"
)

type NotifyMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Tag       string `json:"tag"`
}

type CancelMessage struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type notifyConfig struct {
	GracePeriod   time.Duration
	IdleThreshold time.Duration
}

func defaultNotifyConfig() notifyConfig {
	return notifyConfig{GracePeriod: 5 * time.Second, IdleThreshold: 2 * time.Minute}
}

type notifySessionSnapshot struct {
	Working bool
	Unread  bool
	Alive   bool
	Title   string
	Start   string
}

type pendingCentralNotif struct {
	sessionID string
	notifType string
	title     string
	body      string
	timer     *time.Timer
	notifID   string
}

type activeCentralNotif struct {
	sessionID string
	clientID  string
}

type centralNotifyRouter struct {
	presence *presence.Table
	config   notifyConfig

	mu        sync.Mutex
	prevState map[string]notifySessionSnapshot
	pending   map[string]*pendingCentralNotif
	active    map[string]activeCentralNotif
	nextID    int
}

func newCentralNotifyRouter(p *presence.Table, cfg notifyConfig) *centralNotifyRouter {
	return &centralNotifyRouter{presence: p, config: cfg, prevState: make(map[string]notifySessionSnapshot), pending: make(map[string]*pendingCentralNotif), active: make(map[string]activeCentralNotif)}
}

func (r *centralNotifyRouter) Run(ctx context.Context, seed []sessioncoord.Outcome, events <-chan sessioncoord.Outcome) {
	r.mu.Lock()
	for _, outcome := range seed {
		if outcome.Type != sessioncoord.OutcomeUpserted || outcome.Session == nil {
			continue
		}
		r.prevState[string(outcome.ID)] = notifySnapshot(outcome)
	}
	r.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case outcome, ok := <-events:
			if !ok {
				return
			}
			r.handleOutcome(outcome)
		}
	}
}

func notifySnapshot(o sessioncoord.Outcome) notifySessionSnapshot {
	snap := notifySessionSnapshot{Alive: o.Alive}
	if o.Session != nil {
		snap.Working = o.Session.StatusReported && o.Session.Working
		snap.Unread = o.Session.Unread
		snap.Title = o.Session.Title
		snap.Start = fmtMillisPtr(o.Session.StartedAt)
	}
	return snap
}

func (r *centralNotifyRouter) handleOutcome(o sessioncoord.Outcome) {
	if o.Type == sessioncoord.OutcomeRemoved {
		r.mu.Lock()
		delete(r.prevState, string(o.ID))
		r.mu.Unlock()
		return
	}
	if o.Type != sessioncoord.OutcomeUpserted || o.Session == nil {
		return
	}
	cur := notifySnapshot(o)
	id := string(o.ID)
	r.mu.Lock()
	prev, existed := r.prevState[id]
	r.prevState[id] = cur
	r.mu.Unlock()
	if !existed {
		return
	}
	if prev.Working && !cur.Working && cur.Alive {
		r.scheduleNotification(id, "finished", cur.Title, formatFinishedBodyCentral(cur.Start))
	}
	if !prev.Unread && cur.Unread {
		r.scheduleNotification(id, "unread", cur.Title, "New output")
	}
}

func formatFinishedBodyCentral(start string) string {
	body := "Task finished"
	if start == "" {
		return body
	}
	t, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return body
	}
	dur := time.Since(t).Round(time.Second)
	if dur < time.Minute {
		return fmt.Sprintf("Finished (%ds)", int(dur.Seconds()))
	}
	m := int(dur.Minutes())
	s := int(dur.Seconds()) % 60
	if dur < time.Hour {
		return fmt.Sprintf("Finished (%dm %ds)", m, s)
	}
	h := int(dur.Hours())
	m = m % 60
	return fmt.Sprintf("Finished (%dh %dm)", h, m)
}

func (r *centralNotifyRouter) genID() string {
	r.nextID++
	return fmt.Sprintf("notif-%d", r.nextID)
}

func (r *centralNotifyRouter) scheduleNotification(sessionID, notifType, title, body string) {
	if r.presence.AnyViewing(sessionID) || r.presence.AnyFocused() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.pending[sessionID]; ok {
		if notifType == "finished" && existing.notifType == "unread" {
			existing.notifType = notifType
			existing.title = title
			existing.body = body
		}
		return
	}
	notifID := r.genID()
	p := &pendingCentralNotif{sessionID: sessionID, notifType: notifType, title: title, body: body, notifID: notifID}
	p.timer = time.AfterFunc(r.config.GracePeriod, func() { r.firePending(sessionID) })
	r.pending[sessionID] = p
}

func (r *centralNotifyRouter) firePending(sessionID string) {
	r.mu.Lock()
	p, ok := r.pending[sessionID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.pending, sessionID)
	pendingCount := len(r.pending) + 1
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
	if r.presence.AnyFocused() || r.presence.AnyViewing(sessionID) {
		return
	}
	target := r.presence.BestNotifyTarget(r.config.IdleThreshold)
	if target == nil {
		log.Printf("notify: no target for session %s", sessionID)
		return
	}
	msg := NotifyMessage{Type: "notify", ID: p.notifID, SessionID: sessionID, Title: p.title, Body: p.body, Tag: sessionID}
	r.mu.Lock()
	r.active[p.notifID] = activeCentralNotif{sessionID: sessionID, clientID: target.ID}
	r.mu.Unlock()
	sendNotifyJSON(target.Conn, msg)
	time.AfterFunc(5*time.Minute, func() {
		r.mu.Lock()
		delete(r.active, p.notifID)
		r.mu.Unlock()
	})
}

func (r *centralNotifyRouter) fireCoalesced(count int) {
	target := r.presence.BestNotifyTarget(r.config.IdleThreshold)
	if target == nil {
		log.Printf("notify: no target for coalesced notification (%d sessions)", count)
		return
	}
	r.mu.Lock()
	notifID := r.genID()
	r.mu.Unlock()
	sendNotifyJSON(target.Conn, NotifyMessage{Type: "notify", ID: notifID, Title: "gmux", Body: fmt.Sprintf("%d sessions finished", count), Tag: "coalesced"})
}

func (r *centralNotifyRouter) CancelAllPending() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for sid, p := range r.pending {
		p.timer.Stop()
		delete(r.pending, sid)
	}
}

func (r *centralNotifyRouter) CancelForSession(sessionID string) {
	r.mu.Lock()
	if p, ok := r.pending[sessionID]; ok {
		p.timer.Stop()
		delete(r.pending, sessionID)
	}
	var cancelIDs []string
	for id, active := range r.active {
		if active.sessionID == sessionID {
			cancelIDs = append(cancelIDs, id)
			delete(r.active, id)
		}
	}
	r.mu.Unlock()
	for _, id := range cancelIDs {
		r.broadcastCancel(id)
	}
}

func (r *centralNotifyRouter) broadcastCancel(notifID string) {
	msg := CancelMessage{Type: "cancel", ID: notifID}
	for _, conn := range r.presence.Conns() {
		sendNotifyJSON(conn, msg)
	}
}

func sendNotifyJSON(conn *websocket.Conn, v any) {
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
