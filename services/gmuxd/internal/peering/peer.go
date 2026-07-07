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

	mu             sync.RWMutex
	status         Status
	lastError      string         // human-readable reason for last disconnect
	cachedHealth   SpokeHealth    // peer's /v1/health data, fetched on connect
	healthLoaded   bool           // true after first successful health fetch
	cachedProjects []SpokeProject // peer's projects, refreshed on connect and on projects-update
	projectsLoaded bool
	// cachedDiscovered is the spoke's self-advertised discovered list
	// (host-authoritative; see SpokeDiscovered). Refreshed alongside
	// cachedProjects in fetchProjects.
	cachedDiscovered []SpokeDiscovered

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

	// reconnectBackoff overrides initialBackoff in the run loop.
	// Zero means use initialBackoff. Test-only: lets the reconnect
	// tests run at millisecond cadence instead of real seconds.
	reconnectBackoff time.Duration
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

// CachedProjects returns the peer's project list, derived as
// SpokeProject (slug + launch_cwd hint). Returns false until the
// first successful fetch.
func (p *Peer) CachedProjects() ([]SpokeProject, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cachedProjects, p.projectsLoaded
}

// CachedDiscovered returns the peer's self-advertised discovered list
// (host-authoritative). The bool tracks the same projectsLoaded flag as
// CachedProjects: both are populated by the one fetchProjects call.
func (p *Peer) CachedDiscovered() ([]SpokeDiscovered, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cachedDiscovered, p.projectsLoaded
}

// fetchProjects fetches the spoke's project list via GET /v1/projects,
// projects each Item down to a SpokeProject (slug + launch_cwd hint
// derived from the first path rule), and caches the result. Called
// once after each successful SSE connection and again whenever the
// peer broadcasts projects-update.
func (p *Peer) fetchProjects(ctx context.Context) {
	data, err := p.api.GetProjects(ctx)
	if err != nil {
		log.Printf("peering: %s: fetch projects: %v", p.Config.Name, err)
		return
	}
	var envelope struct {
		Configured []struct {
			Slug  string `json:"slug"`
			Peer  string `json:"peer,omitempty"`
			Match []struct {
				Path string `json:"path,omitempty"`
			} `json:"match"`
		} `json:"configured"`
		Discovered []SpokeDiscovered `json:"discovered"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		log.Printf("peering: %s: parse projects: %v", p.Config.Name, err)
		return
	}
	projects := make([]SpokeProject, 0, len(envelope.Configured))
	for _, it := range envelope.Configured {
		if it.Slug == "" {
			continue
		}
		// Skip reference items (peer field set): we don't surface
		// transitive references in our own snapshot.world. The viewer
		// sees each peer's owned projects, not what those peers in
		// turn reference from further upstream.
		if it.Peer != "" {
			continue
		}
		sp := SpokeProject{Slug: it.Slug}
		for _, r := range it.Match {
			if r.Path != "" {
				sp.LaunchCwd = r.Path
				break
			}
		}
		projects = append(projects, sp)
	}
	// The spoke's discovered list is host-authoritative: it ran its
	// own match rules over its own sessions, so we cache it verbatim
	// rather than recomputing peer discovery blind (ADR 0002/0005).
	discovered := envelope.Discovered
	if discovered == nil {
		discovered = []SpokeDiscovered{}
	}
	p.mu.Lock()
	p.cachedProjects = projects
	p.cachedDiscovered = discovered
	p.projectsLoaded = true
	p.mu.Unlock()
	// Signal a status change so the hub's world coalescer re-emits
	// snapshot.world with the updated peer_projects entry. Reusing
	// peer-status keeps the wire surface minimal; the type-name is
	// a slight overload but the trigger semantics are correct (this
	// peer's externally-visible state changed).
	//
	// Skip the broadcast if the peer's context has been cancelled
	// (peer torn down mid-fetch). The store cleanup that follows
	// disconnect would otherwise race against a stale cache update,
	// and we'd fire a re-compose for a peer the world snapshot no
	// longer enumerates.
	if ctx.Err() != nil {
		return
	}
	if p.onStatus != nil {
		p.onStatus(p.Config.Name, p.status)
	}
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

// Backoff bounds for peer reconnects. The ceiling stays at 30s
// deliberately: peering is a dial-out-only relationship (the hub
// opens the SSE stream; the spoke cannot push "I'm online"), so
// reconnect latency is bounded solely by this interval. A longer
// ceiling would mean a peer that just came online stays invisible
// for minutes. The log spam that motivated issue #244 is handled by
// deduping the disconnect log (see run's lastLogged), not by
// stretching the retry cadence. Transient drops recover fast because
// backoff resets to initialBackoff after any successful connection.
const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
)

// run connects to the spoke's SSE stream and processes events until
// the context is cancelled. Handles reconnection with exponential
// backoff.
func (p *Peer) run(ctx context.Context) {
	minBackoff := initialBackoff
	if p.reconnectBackoff > 0 {
		minBackoff = p.reconnectBackoff
	}
	backoff := minBackoff
	// lastLogged dedupes disconnect logs: repeated identical failures
	// against a down host are logged once, not on every retry. Any
	// change in the failure (or a successful connection in between)
	// logs again.
	lastLogged := ""

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
			backoff = minBackoff
			lastLogged = ""
		}

		msg := fmt.Sprintf("%v", err)
		if msg != lastLogged {
			log.Printf("peering: %s: disconnected: %v (retrying, up to every %s)", p.Config.Name, err, maxBackoff)
			lastLogged = msg
		}

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
			// Also fetch the peer's project list so the hub can surface
			// references to its projects in its own snapshot.world.
			p.fetchProjects(ctx)
		},
		func(ev sseclient.Event) {
			p.handleEvent(ctx, ev.Type, ev.Data)
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

func (p *Peer) handleEvent(ctx context.Context, eventType string, data []byte) {
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

	case "projects-update":
		// Spoke's projects.json changed. Refresh the cached
		// projection so the hub's snapshot.world reflects the new
		// state. Pass the streaming ctx so the fetch is cancelled
		// if the peer disconnects mid-flight (otherwise a slow
		// /v1/projects could race past disconnect and fire a
		// spurious peer-status broadcast via onStatus, triggering
		// a world re-compose on stale data).
		go p.fetchProjects(ctx)

	case "snapshot.world":
		// A `?as=peer` subscription never receives snapshot.world
		// (the spoke only sends it to browser subscribers). Ignore
		// defensively in case that ever changes: the hub composes
		// its own world view authoritatively.

	default:
		// Unknown event types are silently ignored for forward compatibility.
	}
}

// applySessionsSnapshot reconciles the local store's view of this
// peer's sessions against the snapshot. Any session in the snapshot
// is upserted (namespaced) into the store; any session whose Peer
// matches this peer but whose ID is not present in the snapshot is
// removed.
//
// A spoke re-ships its full snapshot on every change (and at its
// coalescer cadence), so the common case is that most sessions in a
// snapshot are identical to what we already hold. We still call
// UpsertRemote for each one; the store suppresses the redundant
// session-upsert broadcast when nothing actually changed (see
// upsertCommon). That dedup lives in the store rather than here
// because only the store sees the fully-normalized session — after
// path canonicalization and unique-slug renumbering — which is what
// a correct equality check has to compare against.
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
		// overwrite the correct title with the adapter-name fallback.
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
