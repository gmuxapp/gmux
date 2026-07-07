package tsauth

import (
	"net/http"
	"strings"
	"sync"
)

// RoutedTransport is an http.RoundTripper that routes requests for hosts
// under the local tailnet's MagicDNS suffix through a tsnet-backed
// transport, and everything else through the fallback (default) transport.
//
// It exists to solve a timing problem: the peering manager is constructed
// at startup, but the tsnet listener becomes ready much later (tailscale
// login can take minutes). A single shared RoutedTransport is handed to
// all peers and the health probe up front; once tsnet is ready, SetTailnet
// installs the suffix and dialer atomically, and peers on the tailnet pick
// up tsnet routing on their next request without re-construction.
//
// A host on a *different* tailnet (reachable only via system tailscaled)
// won't match the local suffix and keeps the default transport, as before.
type RoutedTransport struct {
	fallback http.RoundTripper

	mu     sync.RWMutex
	suffix string // local tailnet MagicDNS suffix, e.g. "tailnet.ts.net"
	tsnet  http.RoundTripper
}

// NewRoutedTransport returns a RoutedTransport that sends everything
// through http.DefaultTransport until SetTailnet is called.
func NewRoutedTransport() *RoutedTransport {
	return &RoutedTransport{fallback: http.DefaultTransport}
}

// SetTailnet installs the tailnet MagicDNS suffix and the tsnet-backed
// transport. Safe to call from any goroutine; requests in flight keep
// whichever transport they already selected.
func (t *RoutedTransport) SetTailnet(suffix string, rt http.RoundTripper) {
	suffix = normalizeHost(suffix)
	t.mu.Lock()
	t.suffix = suffix
	t.tsnet = rt
	t.mu.Unlock()
}

// RoundTrip implements http.RoundTripper.
func (t *RoutedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.pick(req.URL.Hostname()).RoundTrip(req)
}

// pick returns the transport to use for the given request host.
func (t *RoutedTransport) pick(host string) http.RoundTripper {
	t.mu.RLock()
	suffix, ts := t.suffix, t.tsnet
	t.mu.RUnlock()
	if ts != nil && hostOnTailnet(host, suffix) {
		return ts
	}
	return t.fallback
}

// hostOnTailnet reports whether host falls under the MagicDNS suffix:
// either a label under it ("gmux.tailnet.ts.net" for "tailnet.ts.net")
// or the suffix itself. Comparison is case-insensitive and ignores
// trailing dots.
func hostOnTailnet(host, suffix string) bool {
	if suffix == "" {
		return false
	}
	host = normalizeHost(host)
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// normalizeHost lowercases and strips any trailing dot from a DNS name.
func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}
