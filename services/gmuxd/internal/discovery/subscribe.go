package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// Subscriptions tracks active SSE subscriptions to runner /events endpoints.
// One subscription per session — receives status/meta/exit events and updates the store.
type Subscriptions struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc // sessionID → cancel
	store  *store.Store
	// OnExit is called after a session exit event is processed.
	// Returns true if the session was transitioned to resumable
	// (caller should not set exit status).
	OnExit func(sess *store.Session) bool
}

func NewSubscriptions(s *store.Store) *Subscriptions {
	return &Subscriptions{
		active: make(map[string]context.CancelFunc),
		store:  s,
	}
}

// Subscribe starts an SSE subscription to a runner's /events endpoint.
// If already subscribed to this session, this is a no-op.
// The subscription runs in a background goroutine and updates the store
// on status, meta, and exit events.
func (sub *Subscriptions) Subscribe(sessionID, socketPath string) {
	sub.mu.Lock()
	if _, ok := sub.active[sessionID]; ok {
		sub.mu.Unlock()
		return // already subscribed
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub.active[sessionID] = cancel
	sub.mu.Unlock()

	go sub.runSubscription(ctx, sessionID, socketPath)
}

// Unsubscribe cancels and removes the subscription for a session.
func (sub *Subscriptions) Unsubscribe(sessionID string) {
	sub.mu.Lock()
	cancel, ok := sub.active[sessionID]
	if ok {
		delete(sub.active, sessionID)
	}
	sub.mu.Unlock()

	if ok {
		cancel()
	}
}

// UnsubscribeAll cancels all active subscriptions.
func (sub *Subscriptions) UnsubscribeAll() {
	sub.mu.Lock()
	for id, cancel := range sub.active {
		cancel()
		delete(sub.active, id)
	}
	sub.mu.Unlock()
}

// runnerEvent represents an SSE event from a runner's /events endpoint.
// The runner emits: "status" (Status object), "meta" (title/subtitle), "exit" (exit_code).
type runnerEvent struct {
	Type string
	Data json.RawMessage
}

func (sub *Subscriptions) runSubscription(ctx context.Context, sessionID, socketPath string) {
	defer func() {
		sub.mu.Lock()
		delete(sub.active, sessionID)
		sub.mu.Unlock()
	}()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/events", nil)
	if err != nil {
		log.Printf("subscribe: %s: request error: %v", sessionID, err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("subscribe: %s: connect error: %v", sessionID, err)
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("subscribe: %s: unexpected status %d", sessionID, resp.StatusCode)
		return
	}

	log.Printf("subscribe: %s: connected to runner /events", sessionID)

	scanner := bufio.NewScanner(resp.Body)
	var currentEvent string

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if currentEvent != "" {
				sub.handleEvent(sessionID, socketPath, currentEvent, []byte(data))
				currentEvent = ""
			}
			continue
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		log.Printf("subscribe: %s: read error: %v", sessionID, err)
	}

	log.Printf("subscribe: %s: disconnected from runner /events", sessionID)
}

func (sub *Subscriptions) handleEvent(sessionID, socketPath, eventType string, data []byte) {
	sess, ok := sub.store.Get(sessionID)
	if !ok {
		// Session was removed from store — we'll get cleaned up
		return
	}

	switch eventType {
	case "status":
		var status store.Status
		if err := json.Unmarshal(data, &status); err != nil {
			log.Printf("subscribe: %s: bad status event: %v", sessionID, err)
			return
		}
		sess.Status = &status
		sub.store.Upsert(sess)

	case "meta":
		var meta struct {
			ShellTitle   *string `json:"shell_title"`
			AdapterTitle *string `json:"adapter_title"`
			Subtitle     *string `json:"subtitle"`
			Unread       *bool   `json:"unread"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			log.Printf("subscribe: %s: bad meta event: %v", sessionID, err)
			return
		}
		if meta.ShellTitle != nil {
			sess.ShellTitle = *meta.ShellTitle
		}
		if meta.AdapterTitle != nil {
			sess.AdapterTitle = *meta.AdapterTitle
		}
		if meta.Subtitle != nil {
			sess.Subtitle = *meta.Subtitle
		}
		if meta.Unread != nil {
			sess.Unread = *meta.Unread
		}
		sub.store.Upsert(sess)

	case "exit":
		var exit struct {
			ExitCode int `json:"exit_code"`
		}
		if err := json.Unmarshal(data, &exit); err != nil {
			log.Printf("subscribe: %s: bad exit event: %v", sessionID, err)
			return
		}
		sess.Alive = false
		sess.ExitCode = &exit.ExitCode
		sess.ExitedAt = time.Now().UTC().Format(time.RFC3339)
		// Let the OnExit hook set the resume command before upsert.
		// If it returns true, the session transitioned to resumable —
		// don't overwrite with exit status.
		resumed := false
		if sub.OnExit != nil {
			resumed = sub.OnExit(&sess)
		}
		if !resumed {
			if exit.ExitCode == 0 {
				sess.Status = &store.Status{Label: "completed"}
			} else {
				sess.Status = &store.Status{Label: fmt.Sprintf("exited (%d)", exit.ExitCode)}
			}
		}
		sub.store.Upsert(sess)

	default:
		log.Printf("subscribe: %s: unknown event type: %s", sessionID, eventType)
	}
}
