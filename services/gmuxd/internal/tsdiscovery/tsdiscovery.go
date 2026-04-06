// Package tsdiscovery discovers gmuxd instances on the local tailnet.
//
// It subscribes to netmap changes via the tailscale WatchIPNBus API and
// reacts immediately when peers come online, go offline, or leave the
// tailnet. Online peers are probed with a /v1/health request to
// determine whether they're running gmuxd, and discovered instances are
// registered as peers via the peering Manager.
//
// Results are cached to disk so known gmux devices are re-registered
// on restart without re-probing.
package tsdiscovery

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

// PeerManager is the subset of peering.Manager used by the watcher.
type PeerManager interface {
	AddPeer(cfg config.PeerConfig, opts ...peering.PeerOption)
	RemovePeer(name string)
}

// Config configures the tailscale discovery watcher.
type Config struct {
	LocalClient *tailscale.LocalClient
	Transport   http.RoundTripper // tsnet-routed transport
	SelfFQDN    string            // this node's tailnet FQDN (to exclude self)
	Manager     PeerManager
	StateDir    string // directory for the persistent cache file

	// ManualPeerURLs lists URLs of manually-configured peers so we
	// don't auto-discover something the user already set up.
	ManualPeerURLs []string

	// HostnamePrefix is the tsnet hostname prefix used to identify
	// suspected gmux devices that are offline (never probed).
	// Defaults to "gmux".
	HostnamePrefix string
}

// OfflinePeer describes a gmux device visible on the tailnet but
// currently offline or not yet confirmed.
type OfflinePeer struct {
	Name string `json:"name"`
	FQDN string `json:"fqdn"`
}

// Watcher subscribes to tailnet changes and discovers gmuxd instances.
type Watcher struct {
	cfg    Config
	client *http.Client // uses tsnet transport

	mu      sync.Mutex
	state   discoveryState
	offline []OfflinePeer                        // suspected-offline gmux peers
	prev    map[tailcfg.StableNodeID]peerSnapshot // last netmap snapshot

	cancel context.CancelFunc
	done   chan struct{}
}

// peerSnapshot captures the fields we care about from a netmap peer.
type peerSnapshot struct {
	fqdn     string
	hostname string // tailscale HostName (e.g. "gmux-dev")
	online   bool
}

type discoveryState struct {
	// Devices keyed by tailscale stable node ID.
	Devices map[string]*deviceEntry `json:"devices"`
}

type deviceEntry struct {
	FQDN     string `json:"fqdn"`
	PeerName string `json:"peer_name,omitempty"` // set when IsGmux is true
	IsGmux   bool   `json:"is_gmux"`
	ProbedAt string `json:"probed_at"` // RFC 3339
}

// New creates a discovery watcher. Call SetTailscale once the tsnet
// listener is ready, then Start to begin watching.
func New(cfg Config) *Watcher {
	if cfg.HostnamePrefix == "" {
		cfg.HostnamePrefix = "gmux"
	}
	w := &Watcher{
		cfg:    cfg,
		client: &http.Client{Timeout: 3 * time.Second},
		state:  discoveryState{Devices: make(map[string]*deviceEntry)},
	}
	// Load state eagerly so OfflinePeers() works before Start().
	w.loadState()
	return w
}

// OfflinePeers returns tailnet devices that look like gmux instances
// (hostname matches the configured prefix) but are currently offline
// and haven't been confirmed by probing. Updated on each netmap change.
func (w *Watcher) OfflinePeers() []OfflinePeer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offline
}

// SetTailscale configures the tailscale-specific fields after the tsnet
// listener is ready. Must be called before Start.
func (w *Watcher) SetTailscale(lc *tailscale.LocalClient, transport http.RoundTripper, selfFQDN string) {
	w.cfg.LocalClient = lc
	w.cfg.Transport = transport
	w.cfg.SelfFQDN = selfFQDN
	w.client.Transport = transport
}

// Start begins the background event loop. If LocalClient is nil
// (e.g. in tests that only exercise state persistence), the loop
// is not started.
func (w *Watcher) Start() {
	// Re-register known gmux peers immediately (without re-probing).
	// Skip any that are now manually configured (user added a [[peers]]
	// entry after the initial discovery).
	w.mu.Lock()
	for _, d := range w.state.Devices {
		if d.IsGmux && d.PeerName != "" && !w.isManualPeer(d.FQDN) {
			w.cfg.Manager.AddPeer(config.PeerConfig{
				Name: d.PeerName,
				URL:  "https://" + d.FQDN,
			}, peering.WithTransport(w.cfg.Transport))
		}
	}
	w.mu.Unlock()

	if w.cfg.LocalClient == nil {
		return // no watching without a tailscale connection
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})

	go func() {
		defer close(w.done)
		w.run(ctx)
	}()
}

// Stop cancels the event loop and waits for it to finish.
func (w *Watcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
	}
}

func (w *Watcher) run(ctx context.Context) {
	log.Printf("tsdiscovery: tailscale peer discovery enabled")
	for {
		err := w.watchLoop(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("tsdiscovery: watch disconnected: %v; reconnecting in 5s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (w *Watcher) watchLoop(ctx context.Context) error {
	bus, err := w.cfg.LocalClient.WatchIPNBus(ctx, ipn.NotifyInitialNetMap)
	if err != nil {
		return err
	}
	defer bus.Close()

	for {
		n, err := bus.Next()
		if err != nil {
			return err
		}
		if n.NetMap != nil {
			w.handleNetMap(ctx, n.NetMap)
		}
	}
}

// handleNetMap diffs the new netmap against the previous snapshot and
// reacts to changes: probing newly-online peers, removing gone peers,
// and updating the offline list.
func (w *Watcher) handleNetMap(ctx context.Context, nm *netmap.NetworkMap) {
	selfFQDN := normFQDN(w.cfg.SelfFQDN)

	// Build current snapshot.
	curr := make(map[tailcfg.StableNodeID]peerSnapshot, len(nm.Peers))
	for _, nv := range nm.Peers {
		fqdn := normFQDN(nv.Name())
		if fqdn == "" || fqdn == selfFQDN {
			continue
		}
		online := nv.Online().Get()
		hostname := ""
		if nv.Hostinfo().Valid() {
			hostname = nv.Hostinfo().Hostname()
		}
		curr[nv.StableID()] = peerSnapshot{
			fqdn:     fqdn,
			hostname: hostname,
			online:   online,
		}
	}

	w.mu.Lock()
	prev := w.prev
	w.mu.Unlock()

	stateChanged := false

	// Process each peer in the current netmap.
	for id, cs := range curr {
		nodeID := string(id)
		ps, existed := prev[id]

		w.mu.Lock()
		cached := w.state.Devices[nodeID]
		w.mu.Unlock()

		switch {
		case !existed && cs.online:
			// New peer, online: probe it.
			stateChanged = w.handleOnlinePeer(ctx, nodeID, cs) || stateChanged

		case !existed && !cs.online:
			// New peer, offline: nothing to do (offline list rebuilt below).

		case existed && !ps.online && cs.online:
			// Came online. Probe if not already confirmed gmux.
			if cached != nil && cached.IsGmux {
				continue // already managed
			}
			stateChanged = w.handleOnlinePeer(ctx, nodeID, cs) || stateChanged

		case existed && ps.fqdn != cs.fqdn && cs.online:
			// FQDN changed while online: re-probe.
			log.Printf("tsdiscovery: %s FQDN changed (%s -> %s), re-probing", nodeID, ps.fqdn, cs.fqdn)
			stateChanged = w.handleOnlinePeer(ctx, nodeID, cs) || stateChanged

		case existed && cs.online && cached != nil && !cached.IsGmux:
			// Still online, previously probed as non-gmux: re-probe
			// in case gmux was installed or updated since last check.
			// Limit re-probes to once per 5 minutes since netmap updates
			// can arrive frequently.
			if t, err := time.Parse(time.RFC3339, cached.ProbedAt); err == nil && time.Since(t) < 5*time.Minute {
				continue
			}
			stateChanged = w.handleOnlinePeer(ctx, nodeID, cs) || stateChanged
		}
	}

	// Remove peers that left the tailnet entirely.
	for id := range prev {
		if _, still := curr[id]; still {
			continue
		}
		nodeID := string(id)
		w.mu.Lock()
		d := w.state.Devices[nodeID]
		if d != nil && d.IsGmux && d.PeerName != "" {
			log.Printf("tsdiscovery: %s (%s) left the tailnet", d.PeerName, d.FQDN)
			w.cfg.Manager.RemovePeer(d.PeerName)
			delete(w.state.Devices, nodeID)
			stateChanged = true
		}
		w.mu.Unlock()
	}

	// Rebuild offline list: devices matching hostname prefix that are
	// offline and not already confirmed gmux (those are managed peers).
	var offline []OfflinePeer
	for id, cs := range curr {
		if cs.online {
			continue
		}
		hn := cs.hostname
		if !w.looksLikeGmux(hn) {
			continue
		}
		nodeID := string(id)
		w.mu.Lock()
		d := w.state.Devices[nodeID]
		w.mu.Unlock()
		if d != nil && d.IsGmux {
			continue // already a managed peer
		}
		offline = append(offline, OfflinePeer{Name: hn, FQDN: cs.fqdn})
	}

	w.mu.Lock()
	w.prev = curr
	w.offline = offline
	w.mu.Unlock()

	if stateChanged {
		w.saveState()
	}
}

// handleOnlinePeer probes an online device and updates state.
// Returns true if the cache was modified.
func (w *Watcher) handleOnlinePeer(ctx context.Context, nodeID string, cs peerSnapshot) bool {
	// Skip manually-configured peers.
	if w.isManualPeer(cs.fqdn) {
		w.mu.Lock()
		w.state.Devices[nodeID] = &deviceEntry{
			FQDN:     cs.fqdn,
			IsGmux:   false, // don't auto-discover, user manages this
			ProbedAt: time.Now().Format(time.RFC3339),
		}
		w.mu.Unlock()
		return true
	}

	peerName, ok := w.probe(ctx, cs.fqdn)
	entry := &deviceEntry{
		FQDN:     cs.fqdn,
		IsGmux:   ok,
		ProbedAt: time.Now().Format(time.RFC3339),
	}
	if ok {
		entry.PeerName = peerName

		if conflict := w.findNameConflict(nodeID, peerName); conflict != "" {
			log.Printf("tsdiscovery: WARNING: %s (%s) has the same hostname %q as %s; skipping. Give one machine a distinct hostname to resolve this.",
				cs.fqdn, nodeID, peerName, conflict)
		} else {
			log.Printf("tsdiscovery: discovered %s at %s", peerName, cs.fqdn)
			w.cfg.Manager.AddPeer(config.PeerConfig{
				Name: peerName,
				URL:  "https://" + cs.fqdn,
			}, peering.WithTransport(w.cfg.Transport))
		}
	}

	w.mu.Lock()
	w.state.Devices[nodeID] = entry
	w.mu.Unlock()
	return true
}

// looksLikeGmux returns true if the hostname matches the configured
// prefix pattern (e.g. "gmux-dev" matches prefix "gmux").
func (w *Watcher) looksLikeGmux(hostname string) bool {
	if hostname == "" {
		return false
	}
	// Exact match (unlikely for offline — that's us) or prefix-dash.
	return hostname == w.cfg.HostnamePrefix || strings.HasPrefix(hostname, w.cfg.HostnamePrefix+"-")
}

// probe checks whether the device at fqdn is running gmuxd.
// Returns the hostname from the health response and true if it is.
func (w *Watcher) probe(ctx context.Context, fqdn string) (hostname string, ok bool) {
	url := "https://" + fqdn + "/v1/health"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", false
	}

	resp, err := w.client.Do(req)
	if err != nil {
		log.Printf("tsdiscovery: probe %s: %v", fqdn, err)
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("tsdiscovery: probe %s: HTTP %d", fqdn, resp.StatusCode)
		return "", false
	}

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Service  string `json:"service"`
			Hostname string `json:"hostname"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		log.Printf("tsdiscovery: probe %s: bad response: %v", fqdn, err)
		return "", false
	}
	if !envelope.OK || envelope.Data.Service != "gmuxd" {
		return "", false // not gmux, no need to log
	}
	if envelope.Data.Hostname == "" {
		log.Printf("tsdiscovery: probe %s: gmuxd found but hostname field missing (upgrade gmux on that machine)", fqdn)
		return "", false
	}
	return envelope.Data.Hostname, true
}

// findNameConflict checks whether peerName is already used by a
// different device. Returns the conflicting FQDN or empty string.
func (w *Watcher) findNameConflict(nodeID, peerName string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, d := range w.state.Devices {
		if id != nodeID && d.IsGmux && d.PeerName == peerName {
			return d.FQDN
		}
	}
	return ""
}

func (w *Watcher) isManualPeer(fqdn string) bool {
	for _, u := range w.cfg.ManualPeerURLs {
		// Manual URLs are like "https://gmux.tailnet.ts.net" or
		// "https://gmux.tailnet.ts.net/".
		normalized := strings.TrimPrefix(u, "https://")
		normalized = strings.TrimPrefix(normalized, "http://")
		normalized = strings.TrimRight(normalized, "/")
		if normalized == fqdn {
			return true
		}
	}
	return false
}

// normFQDN strips a trailing dot from a tailscale DNS name.
func normFQDN(fqdn string) string {
	return strings.TrimSuffix(strings.TrimSpace(fqdn), ".")
}

// ── State persistence ──

func (w *Watcher) statePath() string {
	return filepath.Join(w.cfg.StateDir, "tailscale-discovery.json")
}

func (w *Watcher) loadState() {
	data, err := os.ReadFile(w.statePath())
	if err != nil {
		return // first run or missing file
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := json.Unmarshal(data, &w.state); err != nil {
		log.Printf("tsdiscovery: corrupt state file, starting fresh: %v", err)
		w.state = discoveryState{Devices: make(map[string]*deviceEntry)}
		return
	}
	if w.state.Devices == nil {
		w.state.Devices = make(map[string]*deviceEntry)
	}
}

func (w *Watcher) saveState() {
	w.mu.Lock()
	data, err := json.MarshalIndent(w.state, "", "  ")
	w.mu.Unlock()
	if err != nil {
		return
	}
	path := w.statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// ── Test helpers ──

// StateForTest returns the full discovery state (for testing).
func (w *Watcher) StateForTest() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := make(map[string]bool, len(w.state.Devices))
	for _, d := range w.state.Devices {
		m[d.FQDN] = d.IsGmux
	}
	return m
}

// SetCacheEntry injects a discovery state entry (for testing).
func (w *Watcher) SetCacheEntry(nodeID, fqdn, peerName string, isGmux bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Devices[nodeID] = &deviceEntry{
		FQDN:     fqdn,
		PeerName: peerName,
		IsGmux:   isGmux,
		ProbedAt: time.Now().Format(time.RFC3339),
	}
}

// ManualPeerURLs returns the configured manual peer URLs.
func ManualPeerURLs(cfg []config.PeerConfig) []string {
	urls := make([]string, len(cfg))
	for i, p := range cfg {
		urls[i] = p.URL
	}
	return urls
}

func init() {
	// Ensure the interface is satisfied at compile time.
	var _ PeerManager = (*peering.Manager)(nil)
}
