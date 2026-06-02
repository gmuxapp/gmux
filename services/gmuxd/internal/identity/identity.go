// Package identity resolves this node's stable, human-readable name
// (ADR 0007): the live tailscale hostname when tailscale is connected,
// otherwise the OS hostname.
//
// This single value feeds the /v1/health `hostname` field (and thus how
// peers name this node in their UI and URLs), so it must be the name the
// node is actually reachable as — not os.Hostname(), which inside a
// container is the ephemeral container ID and churns on recreation.
package identity

import "strings"

// Resolve returns the node identity given the live tailscale FQDN (empty
// when tailscale is disabled or not yet connected) and the OS hostname.
//
// The tailscale name — the first label of the FQDN, e.g. "gmux-laptop"
// from "gmux-laptop.tailnet.ts.net" — wins when present. Tailscale owns
// and persists this name (post-dedup) in its node state, so it is sticky
// across daemon restarts and container recreation. With no tailscale
// FQDN, the OS hostname is used.
func Resolve(tailscaleFQDN, osHostname string) string {
	label := tailscaleFQDN
	if i := strings.IndexByte(tailscaleFQDN, '.'); i >= 0 {
		label = tailscaleFQDN[:i]
	}
	if label != "" {
		return label
	}
	return osHostname
}
