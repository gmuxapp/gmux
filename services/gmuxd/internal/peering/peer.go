package peering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/apiclient"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sseclient"
)

// defaultStreamIdleTimeout is the maximum time the SSE stream can be
// silent before we assume the connection is dead and reconnect. 60s
// is conservative: real events flow every few seconds on an active
// spoke, and on an idle spoke the reconnect is cheap (the disconnect
// prunes this peer's projection and the initial dump on the next
// connect refills it).
const defaultStreamIdleTimeout = 60 * time.Second

// Peer manages the connection to a single remote gmuxd instance.
//
// Protocol primitives (SSE decode, HTTP forwarding, WS proxying) live
// in the apiclient package so peering can focus on the peering-specific
// concerns: namespacing session IDs, ownership filtering, reconnect
// policy, and status reporting.
type Peer struct {
	Config config.PeerConfig
	sink   ProjectionSink
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
	// nil means use the default transport. Set via WithTransport; the
	// hub passes a routed transport that sends same-tailnet MagicDNS
	// hosts through tsnet.
	transport http.RoundTripper

	// streamIdleTimeout overrides the default SSE idle timeout.
	// Zero means use defaultStreamIdleTimeout.
	streamIdleTimeout time.Duration

	// reconnectBackoff overrides initialBackoff in the run loop.
	// Zero means use initialBackoff. Test-only: lets the reconnect
	// tests run at millisecond cadence instead of real seconds.
	reconnectBackoff time.Duration

	// wake shortcuts the reconnect backoff wait. A signal here makes a
	// backing-off peer retry immediately and resets its backoff to the
	// initial interval. Buffered (cap 1) and sent non-blocking, so a
	// signal that arrives while the peer is connected (not waiting)
	// simply queues and is consumed at the next disconnect — harmless.
	// Peering is dial-out only, so this is the only way an external
	// event (e.g. a browser client connecting) can pull a just-online
	// peer in faster than the backoff schedule.
	wake chan struct{}
}

func newPeer(cfg config.PeerConfig, sinkArg any, onStatus func(string, Status), opts ...PeerOption) *Peer {
	sink := projectionSink(sinkArg)
	p := &Peer{
		Config:   cfg,
		sink:     sink,
		status:   StatusDisconnected,
		onStatus: onStatus,
		wake:     make(chan struct{}, 1),
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

// Reconnect signals the run loop to stop waiting out its backoff and
// retry immediately, resetting backoff to the initial interval. Safe
// to call at any time and from any goroutine: the send is
// non-blocking, so it never stalls the caller, and a signal delivered
// while the peer is connected (or already pending) is coalesced.
func (p *Peer) Reconnect() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
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
	// rather than recomputing peer discovery blind (ADR 0002/0025).
	discovered := envelope.Discovered
	if discovered == nil {
		discovered = []SpokeDiscovered{}
	}
	p.mu.Lock()
	unchanged := p.projectsLoaded &&
		reflect.DeepEqual(p.cachedProjects, projects) &&
		reflect.DeepEqual(p.cachedDiscovered, discovered)
	p.cachedProjects = projects
	p.cachedDiscovered = discovered
	p.projectsLoaded = true
	status := p.status
	p.mu.Unlock()
	// Second reciprocal feedback loop: the hub sets projects-update on
	// every world frame it ships to ?as=peer subscribers, so a mutual
	// peer re-fetches /v1/projects, re-broadcasts peer-status, recomposes
	// its own world, and ships projects-update back. Only signal when the
	// cached projection actually changed; a re-fetch that produced the
	// same projects/discovered lists is not externally visible state.
	if unchanged {
		return
	}
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
		p.onStatus(p.Config.Name, status)
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

		// A disconnect prunes this peer's projection
		// (Manager.onStatus -> removePeerSessions), so nothing stale
		// survives the gap and the spoke's initial dump on the next
		// successful connect is always treated as a first delivery
		// (no previous projection, so the no-op gate in
		// managerProjectionSink.ReplacePeerSessions cannot suppress it).

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
		case <-p.wake:
			// External trigger (e.g. a browser client connected):
			// retry now and start over from the initial backoff so a
			// peer that just came online is picked up promptly rather
			// than after a grown wait.
			backoff = minBackoff
			lastLogged = ""
			continue
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
	// Staging belongs to one transport connection. If it disconnects before
	// ready, this local value disappears and no partial projection is visible.
	var bootstrap peerSessionBootstrap

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
			p.handleStreamEvent(ctx, ev.Type, ev.Data, &bootstrap)
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
	Sessions []SessionProjection `json:"sessions"`
}

type sessionStreamMode uint8

const (
	sessionStreamUnknown sessionStreamMode = iota
	sessionStreamLegacy
	sessionStreamV3
	maxPeerSessionDiagnostics = 256
)

type peerSessionBootstrap struct {
	mode        sessionStreamMode
	epoch       uint64
	lastEpoch   uint64
	active      bool
	rows        []SessionProjection
	bytes       int
	diagnostics []sessionstream.Error
}

func (s *peerSessionBootstrap) abandon() {
	s.epoch = 0
	s.active = false
	s.rows = nil
	s.bytes = 0
	s.diagnostics = nil
}

func (p *Peer) handleStreamEvent(ctx context.Context, eventType string, data []byte, staging *peerSessionBootstrap) {
	switch eventType {
	case sessionstream.EventBegin:
		if staging.mode == sessionStreamLegacy {
			log.Printf("peering: %s: ignoring protocol-3 begin on legacy session stream", p.Config.Name)
			return
		}
		var begin sessionstream.Begin
		if err := json.Unmarshal(data, &begin); err != nil || begin.Version != sessionstream.ProtocolVersion || begin.Epoch == 0 {
			staging.abandon()
			log.Printf("peering: %s: bad session bootstrap begin", p.Config.Name)
			return
		}
		if begin.Epoch <= staging.lastEpoch {
			// Ignore replay without destroying a newer in-flight transaction.
			log.Printf("peering: %s: stale session bootstrap epoch %d (last %d)", p.Config.Name, begin.Epoch, staging.lastEpoch)
			return
		}
		staging.abandon()
		staging.mode = sessionStreamV3
		staging.epoch = begin.Epoch
		staging.lastEpoch = begin.Epoch
		staging.active = true
		return
	case sessionstream.EventBatch:
		var batch sessionstream.Batch[SessionProjection]
		if err := json.Unmarshal(data, &batch); err != nil || !staging.active || batch.Epoch != staging.epoch {
			staging.abandon()
			log.Printf("peering: %s: bad or out-of-epoch session bootstrap batch", p.Config.Name)
			return
		}
		if len(staging.rows)+len(batch.Sessions) > sessionstream.MaxStagedRows || staging.bytes+len(data) > sessionstream.MaxStagedBytes {
			staging.abandon()
			log.Printf("peering: %s: session bootstrap exceeds staging limit", p.Config.Name)
			return
		}
		staging.rows = append(staging.rows, batch.Sessions...)
		staging.bytes += len(data)
		return
	case sessionstream.EventReady:
		var ready sessionstream.Ready
		if err := json.Unmarshal(data, &ready); err != nil || !staging.active || ready.Epoch != staging.epoch {
			staging.abandon()
			log.Printf("peering: %s: bad or out-of-epoch session bootstrap ready", p.Config.Name)
			return
		}
		rows := staging.rows
		staging.abandon()
		p.applySessionsSnapshot(rows)
		return
	case sessionstream.EventError:
		// Diagnostics quarantine individual rows; they do not invalidate the
		// transaction or prevent the remaining rows from reaching ready.
		var diagnostic sessionstream.Error
		if err := json.Unmarshal(data, &diagnostic); err != nil {
			log.Printf("peering: %s: bad session bootstrap diagnostic", p.Config.Name)
			return
		}
		id, message := diagnostic.ID, diagnostic.Message
		if len(id) > 256 {
			id = id[:256] + "…"
		}
		if len(message) > 512 {
			message = message[:512] + "…"
		}
		diagnostic.ID, diagnostic.Message = id, message
		if staging.active && diagnostic.Epoch == staging.epoch && len(staging.diagnostics) < maxPeerSessionDiagnostics {
			staging.diagnostics = append(staging.diagnostics, diagnostic)
		}
		log.Printf("peering: %s: session %q omitted: %q (%s, count=%d)", p.Config.Name, id, message, diagnostic.Code, max(diagnostic.Count, 1))
		return
	case "snapshot.sessions":
		if staging.mode == sessionStreamV3 {
			log.Printf("peering: %s: ignoring legacy snapshot on protocol-3 session stream", p.Config.Name)
			return
		}
		var payload sseSnapshotSessions
		if err := json.Unmarshal(data, &payload); err != nil {
			log.Printf("peering: %s: bad snapshot.sessions: %v", p.Config.Name, err)
			return
		}
		staging.abandon()
		staging.mode = sessionStreamLegacy
		p.applySessionsSnapshot(payload.Sessions)
		return
	}
	p.handleEvent(ctx, eventType, data)
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
		// no longer appear (handles dismiss, kill, conversation takeover that
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
		if p.sink != nil {
			p.sink.SessionActivity(namespacedID)
		}

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
// coalescer cadence), so the common case is that the whole snapshot is
// identical to what we already hold. We still hand every delivery down
// unconditionally; the no-op dedup lives one layer below, in
// managerProjectionSink.ReplacePeerSessions (see projectionsEqual),
// because that is the only place that holds both the previous and the
// new projection. Dropping that guard makes two mutually-peered nodes
// ping-pong identical snapshots forever — it is load-bearing, not an
// optimization.
//
// What this function must do first is normalize identity, since the
// gate below compares whole rows: IDs are namespaced, Peer is stamped,
// and SocketPath is cleared, so nothing peer-local can read as a change.
func (p *Peer) applySessionsSnapshot(input any) {
	var remote []SessionProjection
	switch rows := input.(type) {
	case []SessionProjection:
		remote = rows
	default:
		b, _ := json.Marshal(rows)
		_ = json.Unmarshal(b, &remote)
	}
	out := make([]SessionProjection, 0, len(remote))
	for i := range remote {
		sess := cloneProjection(remote[i])
		if p.isForwardedFromKnownOrigin(sess.ID) {
			continue
		}
		sess.ID = NamespaceID(sess.ID, p.Config.Name)
		// Bare parent IDs are owned by the same origin as the child and must
		// enter the viewer namespace with it. Already-qualified references name
		// another host (or were explicitly qualified by the origin), so retain
		// them rather than manufacturing parent@other@this-peer.
		if sess.ParentSessionID != "" {
			if _, parentPeer := ParseID(sess.ParentSessionID); parentPeer == "" {
				sess.ParentSessionID = NamespaceID(sess.ParentSessionID, p.Config.Name)
			}
		}
		sess.Peer = p.Config.Name
		sess.SocketPath = ""
		out = append(out, sess)
	}
	if p.sink != nil {
		p.sink.ReplacePeerSessions(p.Config.Name, out)
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
