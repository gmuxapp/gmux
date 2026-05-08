package peering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/apiclient"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sseclient"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// defaultStreamIdleTimeout is the maximum time the SSE stream can be
// silent before we assume the connection is dead and reconnect. 60s
// is conservative: real events flow every few seconds on an active
// spoke, and on an idle spoke the reconnect is invisible (sessions
// stay in the store, the initial dump produces no-op upserts).
const defaultStreamIdleTimeout = 60 * time.Second

// Peer manages the connection to a single remote gmuxd instance.
//
// Protocol primitives (SSE decode, HTTP forwarding, WS proxying) live
// in the apiclient package so peering can focus on the peering-specific
// concerns: namespacing session IDs, ownership filtering, reconnect
// policy, and status reporting.
type Peer struct {
	Config config.PeerConfig
	store  *store.Store
	api    *apiclient.Client

	mu           sync.RWMutex
	status       Status
	lastError    string       // human-readable reason for last disconnect
	cachedHealth SpokeHealth  // peer's /v1/health data, fetched on connect
	healthLoaded bool         // true after first successful health fetch

	// onStatus is called when connection state changes.
	onStatus func(name string, status Status)

	// isKnownOrigin reports whether a peer name refers to this node or
	// another peer we're directly connected to. Used to drop forwarded
	// sessions that we can reach via a shorter path (or that are our
	// own sessions echoed back through a mutual subscription).
	isKnownOrigin func(name string) bool

	// transport is the HTTP round-tripper for all spoke connections.
	// nil means use the default transport. Set via WithTransport for
	// tailscale-discovered peers that route through tsnet.
	transport http.RoundTripper

	// streamIdleTimeout overrides the default SSE idle timeout.
	// Zero means use defaultStreamIdleTimeout.
	streamIdleTimeout time.Duration
}

func newPeer(cfg config.PeerConfig, st *store.Store, onStatus func(string, Status), opts ...PeerOption) *Peer {
	p := &Peer{
		Config:   cfg,
		store:    st,
		status:   StatusDisconnected,
		onStatus: onStatus,
	}
	for _, o := range opts {
		o(p)
	}
	// Construct the API client after options have been applied so a
	// WithTransport option propagates into it.
	apiOpts := []apiclient.Option{apiclient.WithBearerToken(cfg.Token)}
	if p.transport != nil {
		apiOpts = append(apiOpts, apiclient.WithTransport(p.transport))
	}
	// Idle timeout: detect silent network drops on the SSE stream.
	idleTimeout := defaultStreamIdleTimeout
	if p.streamIdleTimeout > 0 {
		idleTimeout = p.streamIdleTimeout
	}
	apiOpts = append(apiOpts, apiclient.WithStreamIdleTimeout(idleTimeout))
	p.api = apiclient.New(cfg.URL, apiOpts...)
	return p
}

// Status returns the current connection state.
func (p *Peer) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

// LastError returns a human-readable reason for the last disconnect.
func (p *Peer) LastError() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastError
}

func (p *Peer) setStatus(s Status) {
	p.mu.Lock()
	old := p.status
	p.status = s
	if s == StatusConnected {
		p.lastError = ""
	}
	p.mu.Unlock()

	if old != s && p.onStatus != nil {
		p.onStatus(p.Config.Name, s)
	}
}

// Forward proxies an HTTP request to the spoke's session action
// endpoint, stripping the peer namespace from the session ID. The
// spoke sees the original (non-namespaced) session ID.
func (p *Peer) Forward(w http.ResponseWriter, r *http.Request, originalID, action string) {
	p.api.ForwardAction(w, r, originalID, action)
}

// ForwardLaunch sends a launch request to the spoke. The top-level
// "peer" field is stripped before forwarding so the spoke treats the
// request as a local launch.
func (p *Peer) ForwardLaunch(w http.ResponseWriter, r *http.Request) {
	p.api.ForwardLaunch(w, r)
}

// ForwardPath proxies an arbitrary HTTP request to the spoke at the
// given absolute path. Used by the generic peer proxy at
// /v1/peers/{peer}/... so a hub can mutate state that lives on a
// spoke (e.g., reorder a peer's projects.json) without the hub
// having to mirror or re-implement that state locally (ADR 0002).
func (p *Peer) ForwardPath(w http.ResponseWriter, r *http.Request, path string) {
	p.api.ForwardPath(w, r, path)
}

// CachedHealth returns the spoke's cached health data. The second
// return value is false if health has not been fetched yet.
func (p *Peer) CachedHealth() (SpokeHealth, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cachedHealth, p.healthLoaded
}

// fetchHealth fetches the spoke's /v1/health via apiclient, extracts
// version and launcher info, and caches the result. Called once after
// each successful SSE connection.
func (p *Peer) fetchHealth(ctx context.Context) {
	data, err := p.api.GetHealth(ctx)
	if err != nil {
		log.Printf("peering: %s: fetch health: %v", p.Config.Name, err)
		return
	}
	var h SpokeHealth
	if err := json.Unmarshal(data, &h); err != nil {
		log.Printf("peering: %s: parse health: %v", p.Config.Name, err)
		return
	}
	p.mu.Lock()
	p.cachedHealth = h
	p.healthLoaded = true
	p.mu.Unlock()
}

// ProxyWS proxies a browser WebSocket connection to the spoke's
// /ws/{sessionID} endpoint. The hub accepts the browser WS, the
// apiclient dials the spoke WS with bearer auth and pipes the two
// connections bidirectionally with direction-specific read limits
// (256 KiB client, 4 MiB spoke) that accommodate large terminal
// snapshots.
func (p *Peer) ProxyWS(w http.ResponseWriter, r *http.Request, originalID string) {
	log.Printf("peering: %s: ws proxying %s", p.Config.Name, originalID)
	p.api.ProxyWS(w, r, originalID)
}

// run connects to the spoke's SSE stream and processes events until
// the context is cancelled. Handles reconnection with exponential
// backoff.
func (p *Peer) run(ctx context.Context) {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		p.setStatus(StatusConnecting)
		wasConnected := false
		err := p.subscribe(ctx, func() { wasConnected = true })

		// Sessions stay in the store across reconnects. The spoke's
		// initial dump on the next successful connect will upsert
		// current state; anything the spoke no longer reports stays
		// as stale-but-visible until the user dismisses it.
		// RemoveByPeer only runs on intentional peer removal (see
		// Manager.removePeer).

		if err != nil && ctx.Err() == nil {
			p.mu.Lock()
			p.lastError = categorizeError(err)
			p.mu.Unlock()
		}
		// Keep cachedHealth across reconnects: the spoke's version
		// and launchers don't change because our connection dropped,
		// and clearing it would make the UI show empty data during
		// the brief reconnect window.
		p.setStatus(StatusDisconnected)

		if ctx.Err() != nil {
			return
		}

		// Reset backoff after a successful connection so transient drops
		// reconnect quickly instead of carrying over stale backoff.
		if wasConnected {
			backoff = initialBackoff
		}

		log.Printf("peering: %s: disconnected: %v (retry in %s)", p.Config.Name, err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// subscribe connects to the spoke and processes its SSE stream via
// apiclient. The onConnected callback fires once after a successful
// connection, allowing the caller to track whether the connection was
// established (used to decide whether to reset backoff).
func (p *Peer) subscribe(ctx context.Context, onConnected func()) error {
	sse := p.api.Events()

	err := sse.Subscribe(ctx,
		func() {
			p.setStatus(StatusConnected)
			log.Printf("peering: %s: connected to %s/v1/events", p.Config.Name, p.Config.URL)
			if onConnected != nil {
				onConnected()
			}
			// Fetch the peer's health once per connection so the hub
			// can serve version and launcher data from cache.
			p.fetchHealth(ctx)
		},
		func(ev sseclient.Event) {
			p.handleEvent(ev.Type, ev.Data)
		},
	)

	// Normalize errors so run() + categorizeError see the same shapes
	// they did before the apiclient migration.
	switch {
	case err == nil:
		return fmt.Errorf("stream ended")
	case errors.Is(err, sseclient.ErrStreamEnded):
		return fmt.Errorf("stream ended")
	case errors.Is(err, sseclient.ErrStreamIdle):
		return fmt.Errorf("no data received")
	case errors.Is(err, sseclient.ErrUnauthorized):
		return fmt.Errorf("auth failed: %w", err)
	default:
		return err
	}
}

// sseActivity is the wire format for the bare session-activity event.
type sseActivity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// sseSnapshotSessions is the wire format for snapshot.sessions.
type sseSnapshotSessions struct {
	Sessions []store.Session `json:"sessions"`
}

// sseEvent is the wire format for protocol-1 per-event SSE
// (session-upsert / session-remove). Retained so a v2 hub can
// consume sessions from a v1.x spoke that doesn't emit
// snapshot.sessions. v2 spokes suppress these for `?as=peer`
// subscribers, so this path only fires against v1 spokes in
// steady state.
type sseEvent struct {
	Type    string           `json:"type"`
	ID      string           `json:"id"`
	Session *json.RawMessage `json:"session,omitempty"`
}

// isForwardedFromKnownOrigin checks whether a session ID (before
// namespacing) was forwarded from a peer we can reach directly.
// Returns true if the session should be dropped.
func (p *Peer) isForwardedFromKnownOrigin(id string) bool {
	if p.isKnownOrigin == nil {
		return false
	}
	_, innerPeer := ParseID(id)
	return innerPeer != "" && p.isKnownOrigin(innerPeer)
}

func (p *Peer) handleEvent(eventType string, data []byte) {
	switch eventType {
	case "snapshot.sessions":
		// Authoritative replacement: the spoke's view of its owned
		// sessions. We mirror it into the local store namespaced by
		// peer name and remove any local entries for this peer that
		// no longer appear (handles dismiss, kill, slug takeover that
		// happened on the spoke).
		var payload sseSnapshotSessions
		if err := json.Unmarshal(data, &payload); err != nil {
			log.Printf("peering: %s: bad snapshot.sessions: %v", p.Config.Name, err)
			return
		}
		p.applySessionsSnapshot(payload.Sessions)

	case "session-activity":
		var ev sseActivity
		if err := json.Unmarshal(data, &ev); err != nil {
			return
		}
		if p.isForwardedFromKnownOrigin(ev.ID) {
			return
		}
		namespacedID := NamespaceID(ev.ID, p.Config.Name)
		p.store.Broadcast(store.Event{
			Type: "session-activity",
			ID:   namespacedID,
		})

	case "session-upsert":
		// v1 compat: a v1.x spoke ships sessions one at a time via
		// session-upsert. A v2 spoke that knows about `?as=peer`
		// suppresses these in favour of snapshot.sessions. Applying
		// both from a misconfigured v2 spoke is idempotent: the
		// upsert produces the same store state the snapshot would.
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("peering: %s: bad upsert event: %v", p.Config.Name, err)
			return
		}
		if ev.Session == nil {
			return
		}
		if p.isForwardedFromKnownOrigin(ev.ID) {
			return
		}
		var sess store.Session
		if err := json.Unmarshal(*ev.Session, &sess); err != nil {
			log.Printf("peering: %s: bad session payload: %v", p.Config.Name, err)
			return
		}
		sess.ID = NamespaceID(ev.ID, p.Config.Name)
		sess.Peer = p.Config.Name
		sess.SocketPath = "" // meaningless on hub side
		// UpsertRemote (not Upsert) because the spoke already resolved
		// Title and Resumable; rerunning resolveTitle here would
		// clobber the correct title with the Kind fallback.
		p.store.UpsertRemote(sess)

	case "session-remove":
		// v1 compat: paired with session-upsert above.
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("peering: %s: bad remove event: %v", p.Config.Name, err)
			return
		}
		if p.isForwardedFromKnownOrigin(ev.ID) {
			return
		}
		p.store.Remove(NamespaceID(ev.ID, p.Config.Name))

	case "snapshot.world", "projects-update", "peer-status":
		// Hub composes its own world view. v2 spokes don't emit
		// snapshot.world to asPeer subscribers anyway; v1 spokes
		// emit projects-update / peer-status which we don't care
		// about (hub state is authoritative for those).

	default:
		// Unknown event types are silently ignored for forward compatibility.
	}
}

// applySessionsSnapshot reconciles the local store's view of this
// peer's sessions against the snapshot. Any session in the snapshot
// is upserted (namespaced) into the store; any session whose Peer
// matches this peer but whose ID is not present in the snapshot is
// removed.
func (p *Peer) applySessionsSnapshot(remote []store.Session) {
	seen := make(map[string]bool, len(remote))
	for i := range remote {
		sess := remote[i]
		if p.isForwardedFromKnownOrigin(sess.ID) {
			// A→B→A loop: B is shipping us back a session whose
			// origin we already reach directly. Skip.
			continue
		}
		namespacedID := NamespaceID(sess.ID, p.Config.Name)
		seen[namespacedID] = true
		sess.ID = namespacedID
		sess.Peer = p.Config.Name
		sess.SocketPath = "" // meaningless on hub side
		// UpsertRemote (not Upsert) because the spoke already resolved
		// Title and Resumable. Upsert would re-run resolveTitle against
		// the wire session where ShellTitle/AdapterTitle are absent
		// (they're internal fields, intentionally off the wire) and
		// overwrite the correct title with the Kind fallback.
		p.store.UpsertRemote(sess)
	}

	// Removal pass: anything we still have for this peer that the
	// snapshot omitted has either been dismissed, killed, or slug-
	// renamed on the origin side.
	for _, s := range p.store.List() {
		if s.Peer != p.Config.Name {
			continue
		}
		if !seen[s.ID] {
			p.store.Remove(s.ID)
		}
	}
}

// categorizeError returns a short, user-friendly description of a peer
// connection failure. Intended for display in the UI, not for logs.
func categorizeError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "auth failed"):
		return "authentication failed"
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "no such host"):
		return "host not found"
	case strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "context deadline exceeded"):
		return "connection timed out"
	case strings.Contains(s, "certificate"),
		strings.Contains(s, "x509"):
		return "TLS certificate error"
	case strings.Contains(s, "no data received"):
		return "no data received"
	case strings.Contains(s, "stream ended"):
		return "connection lost"
	default:
		return "connection failed"
	}
}
