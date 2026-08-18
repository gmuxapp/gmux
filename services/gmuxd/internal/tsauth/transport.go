package tsauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/tsaddr"
)

// PeerClient is the supported LocalAPI seam used both to inspect a tailscaled
// netmap and to dial through that same tailscaled instance. Binding membership
// and dialing to one client avoids assuming a kernel TUN route exists (tsnet
// and userspace-networking nodes deliberately do not provide one).
type PeerClient interface {
	Status(context.Context) (*ipnstate.Status, error)
	DialTCP(context.Context, string, uint16) (net.Conn, error)
}

type statusClient interface {
	Status(context.Context) (*ipnstate.Status, error)
}

func statusWithTimeout(ctx context.Context, client statusClient, timeout time.Duration) (*ipnstate.Status, error) {
	statusCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.Status(statusCtx)
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// RoutedTransport routes local-tailnet MagicDNS names through embedded tsnet.
// Foreign .ts.net names remain on the system network, but when system DNS does
// not know a shared node their address is taken from system tailscaled's
// LocalAPI. The URL host is never rewritten, preserving TLS verification/SNI.
type RoutedTransport struct {
	fallback http.RoundTripper
	foreign  http.RoundTripper

	mu     sync.RWMutex
	suffix string
	tsnet  http.RoundTripper
}

// NewRoutedTransport starts with ordinary network behavior. Once gmux's
// embedded tsnet is ready, SetTailnet installs its LocalAPI client for exact
// foreign-peer membership and dialing. gmux deliberately does not depend on a
// separate system tailscaled, whose identity/state may be unrelated or stale.
func NewRoutedTransport() *RoutedTransport {
	return newRoutedTransport(http.DefaultTransport, nil, &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	})
}

func newRoutedTransport(fallback http.RoundTripper, client PeerClient, dialer contextDialer) *RoutedTransport {
	t := &RoutedTransport{fallback: fallback}
	if base, ok := fallback.(*http.Transport); ok {
		exact := base.Clone()
		// An HTTP proxy would have to resolve the foreign MagicDNS name and
		// DialContext would see only the proxy address. Exact netmap members
		// therefore bypass proxies; unknown names retain fallback behavior.
		exact.Proxy = nil
		// Re-check LocalAPI and establish a fresh socket for every request.
		// Long-lived SSE requests are unaffected, while later REST/reconnect
		// requests cannot reuse a socket admitted under stale map state.
		exact.DisableKeepAlives = true
		exact.DialContext = (&peerAddressDialer{fallback: dialer}).DialContext
		t.foreign = &foreignPeerTransport{fallback: fallback, exact: exact, client: client}
	}
	return t
}

// SetTailnet installs the local tailnet suffix, embedded-tsnet transport, and
// its LocalAPI client atomically. Foreign shared peers are then resolved and
// dialed through embedded tsnet. A nil client retains the currently installed
// client; this is useful when only later suffix discovery is being updated.
func (t *RoutedTransport) SetTailnet(suffix string, rt http.RoundTripper, client PeerClient) {
	t.mu.Lock()
	t.suffix = normalizeHost(suffix)
	t.tsnet = rt
	if foreign, ok := t.foreign.(*foreignPeerTransport); ok && client != nil {
		foreign.setClient(client)
	}
	t.mu.Unlock()
}

func (t *RoutedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.pick(req.URL.Hostname()).RoundTrip(req)
}

func (t *RoutedTransport) pick(host string) http.RoundTripper {
	t.mu.RLock()
	suffix, ts := t.suffix, t.tsnet
	t.mu.RUnlock()
	if ts != nil && hostOnTailnet(host, suffix) {
		return ts
	}
	if t.foreign != nil && isTSNetName(host) {
		return t.foreign
	}
	return t.fallback
}

func hostOnTailnet(host, suffix string) bool {
	if suffix == "" {
		return false
	}
	host = normalizeHost(host)
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

func isTSNetName(host string) bool {
	host = normalizeHost(host)
	return strings.HasSuffix(host, ".ts.net") && host != "ts.net"
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(h), "."))
}

const (
	peerStatusFreshFor = 2 * time.Second
	peerStatusStaleFor = 30 * time.Second
)

type statusFlight struct {
	generation uint64
	done       chan struct{}
	status     *ipnstate.Status
	err        error
}

type foreignPeerTransport struct {
	fallback http.RoundTripper
	exact    http.RoundTripper

	mu         sync.Mutex
	client     PeerClient
	generation uint64
	cached     *ipnstate.Status
	cachedAt   time.Time
	cachedGen  uint64
	inflight   *statusFlight
	now        func() time.Time
}

func (t *foreignPeerTransport) setClient(client PeerClient) {
	t.mu.Lock()
	t.client = client
	t.generation++
	t.cached = nil
	t.cachedAt = time.Time{}
	t.cachedGen = 0
	t.mu.Unlock()
}

func (t *foreignPeerTransport) currentClient() (PeerClient, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.client, t.generation
}

func (t *foreignPeerTransport) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *foreignPeerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	client, generation := t.currentClient()
	if client == nil {
		return t.fallback.RoundTrip(req)
	}
	status, err := t.peerStatus(req.Context(), client, generation)
	if err != nil {
		// Once embedded tsnet is authoritative, never send a peer bearer token
		// to a DNS result we could not verify against its netmap.
		return nil, fmt.Errorf("tailscale peer status unavailable: %w", err)
	}
	peers := exactPeers(status, req.URL.Hostname())
	if len(peers) == 0 {
		// Unknown names never receive privileged address substitution or proxy
		// bypass. Preserve ordinary routing for legitimate public/system-DNS
		// .ts.net URLs; HTTPS still verifies the original hostname before any
		// bearer-token request is delivered.
		return t.fallback.RoundTrip(req)
	}
	if len(peers) > 1 {
		return nil, fmt.Errorf("tailscale peer %q is ambiguous in the local netmap", normalizeHost(req.URL.Hostname()))
	}
	peer := peers[0]
	if !peer.Online {
		// Fail closed: falling through DNS could reach a different endpoint
		// under a name tailscaled identifies as this currently-offline peer.
		return nil, fmt.Errorf("tailscale peer %q is offline (last seen %s)", normalizeHost(req.URL.Hostname()), peer.LastSeen.Format(time.RFC3339))
	}
	ips := usableIPs(peer.TailscaleIPs)
	if len(ips) == 0 {
		return nil, fmt.Errorf("tailscale peer %q has no dialable Tailscale address", normalizeHost(req.URL.Hostname()))
	}
	ctx := context.WithValue(req.Context(), peerAddressesKey{}, peerDialTarget{client: client, ips: ips})
	clone := req.Clone(ctx)
	return t.exact.RoundTrip(clone)
}

// peerStatus coalesces concurrent reconnect bursts and tolerates a short
// LocalAPI/backend stall using a recent immutable snapshot. Membership may be
// at most peerStatusStaleFor old; beyond that, callers fail closed.
func (t *foreignPeerTransport) peerStatus(ctx context.Context, client PeerClient, generation uint64) (*ipnstate.Status, error) {
	now := t.clock()
	t.mu.Lock()
	if t.cached != nil && t.cachedGen == generation && now.Sub(t.cachedAt) <= peerStatusFreshFor {
		status := t.cached
		t.mu.Unlock()
		return status, nil
	}
	flight := t.inflight
	leader := flight == nil || flight.generation != generation
	if leader {
		flight = &statusFlight{generation: generation, done: make(chan struct{})}
		t.inflight = flight
	}
	t.mu.Unlock()

	if leader {
		// The shared refresh must not inherit whichever request happened to
		// become leader; one canceled peer must not poison healthy waiters.
		go t.refreshPeerStatus(client, generation, flight)
	}
	select {
	case <-flight.done:
		return flight.status, flight.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *foreignPeerTransport) refreshPeerStatus(client PeerClient, generation uint64, flight *statusFlight) {
	status, err := statusWithTimeout(context.Background(), client, 5*time.Second)
	finishedAt := t.clock()
	t.mu.Lock()
	if err == nil {
		if t.generation == generation {
			t.cached = status
			t.cachedAt = finishedAt
			t.cachedGen = generation
		}
		flight.status = status
	} else if t.cached != nil && t.cachedGen == generation && finishedAt.Sub(t.cachedAt) <= peerStatusStaleFor {
		flight.status = t.cached
		flight.err = nil
	} else {
		flight.err = err
	}
	if t.inflight == flight {
		t.inflight = nil
	}
	close(flight.done)
	t.mu.Unlock()
}

type peerAddressesKey struct{}

type peerDialTarget struct {
	client PeerClient
	ips    []netip.Addr
}

type peerAddressDialer struct {
	fallback contextDialer
}

func (d *peerAddressDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return d.fallback.DialContext(ctx, network, address)
	}
	target, _ := ctx.Value(peerAddressesKey{}).(peerDialTarget)
	if target.client == nil || len(target.ips) == 0 {
		return d.fallback.DialContext(ctx, network, address)
	}
	return dialAny(ctx, localAPIDialer{client: target.client}, network, port, target.ips)
}

type localAPIDialer struct{ client PeerClient }

func (d localAPIDialer) DialContext(ctx context.Context, _ string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	portNumber, err := net.LookupPort("tcp", port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("invalid peer port %q", port)
	}
	// Persistent SSE requests do not carry a connect deadline. Bound the
	// LocalAPI upgrade/dial phase like http.DefaultTransport's dialer; the
	// returned connection outlives this setup context.
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return d.client.DialTCP(dialCtx, host, uint16(portNumber))
}

type dialResult struct {
	conn net.Conn
	err  error
}

// dialAny races all current Tailscale addresses. This gives dual-stack peers
// the same essential property as net.Dialer's Happy Eyeballs implementation:
// a blackholed address cannot consume the request's entire deadline before a
// reachable address family is attempted.
func dialAny(ctx context.Context, dialer contextDialer, network, port string, ips []netip.Addr) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dialResult, len(ips))
	for _, ip := range ips {
		ip := ip
		go func() {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			results <- dialResult{conn: conn, err: err}
		}()
	}
	var errs []string
	for i := 0; i < len(ips); i++ {
		select {
		case result := <-results:
			if result.err == nil {
				cancel()
				// Losing racers can also have connected before cancellation;
				// drain and close them rather than leaking sockets.
				drainDialResults(results, len(ips)-i-1)
				return result.conn, nil
			}
			errs = append(errs, result.err.Error())
		case <-ctx.Done():
			// A dial can complete successfully concurrently with cancellation.
			// Every result not returned to the caller must still be closed.
			drainDialResults(results, len(ips)-i)
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("dial failed: %s", strings.Join(errs, "; "))
}

func drainDialResults(results <-chan dialResult, count int) {
	if count == 0 {
		return
	}
	go func() {
		for range count {
			result := <-results
			if result.conn != nil {
				_ = result.conn.Close()
			}
		}
	}()
}

func exactPeers(status *ipnstate.Status, host string) []*ipnstate.PeerStatus {
	if status == nil {
		return nil
	}
	want := normalizeHost(host)
	var found []*ipnstate.PeerStatus
	for _, peer := range status.Peer {
		if peer != nil && normalizeHost(peer.DNSName) == want {
			found = append(found, peer)
		}
	}
	return found
}

func usableIPs(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs))
	seen := make(map[netip.Addr]bool, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.IsValid() || !tsaddr.IsTailscaleIP(addr) || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}
