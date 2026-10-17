package tsauth

import (
	"fmt"
	"time"
)

// SelfReach is gmuxd's OWN tailscale reachability: can this host be reached
// over the tailnet right now, and if not, why and what to do about it. It is
// derived on demand from the embedded tsnet node's LocalAPI status (the same
// reading Diag already takes) — no subprocess, no system tailscaled, no cache,
// no peer accounting. Whether a specific peer is reachable is a different
// question and is deliberately not answered here.
type SelfReach struct {
	// State is the stable classification (see the Reach* constants).
	State string `json:"state"`
	// Line is the one human-readable sentence `gmux daemon status` prints.
	Line string `json:"line"`
	// URL is the https tailnet URL, present when the node is reachable.
	URL string `json:"url,omitempty"`
	// AuthURL is the node's own login URL, present when control offers one.
	AuthURL string `json:"auth_url,omitempty"`
	// KeyExpiry is the self node key expiry (RFC3339), present when known.
	KeyExpiry string `json:"key_expiry,omitempty"`
}

// Reach states. These strings are published on /v1/health as
// tailscale_self.state, so treat them as wire contract.
const (
	// ReachReachable — serving at a tailnet name with a certificate.
	ReachReachable = "reachable"
	// ReachNoHTTPS — logged in and running, but the tailnet issues no HTTPS
	// certificates, so tsnet can never serve (a config problem, not auth).
	ReachNoHTTPS = "https_disabled"
	// ReachKeyExpired — the node key expired; the tailnet stops routing to
	// this node until it re-authenticates.
	ReachKeyExpired = "key_expired"
	// ReachNeedsLogin — the node has no usable session and control is
	// offering a login URL.
	ReachNeedsLogin = "needs_login"
	// ReachStarting — the node exists and is coming up (transient).
	ReachStarting = "starting"
	// ReachStopped — the node's backend is stopped.
	ReachStopped = "stopped"
	// ReachUnavailable — the node's LocalAPI did not answer; its state is
	// unknown, which is not the same as "not configured".
	ReachUnavailable = "unavailable"
	// ReachDisabled — remote access is off; there is no tsnet node.
	ReachDisabled = "disabled"
)

// reAuthHint names the re-auth path for gmuxd's OWN node. A `tailscale up`
// hint would be wrong: this node is an embedded tsnet node with its own state
// dir and LocalAPI, so the system tailscale CLI does not own it. tsnet's own
// guidance is to restart with TS_AUTHKEY set or open the login URL, and
// gmuxd's user-facing route to that URL is `gmux remote`, which reads the
// daemon's health and prints it.
const reAuthHint = "run 'gmux remote' and open the login URL it prints"

// keyExpiryLineFormat renders the expiry time inside a status line; the JSON
// field keeps machine-readable RFC3339.
const keyExpiryLineFormat = "2006-01-02 15:04 MST"

// SelfReachDisabled is the state for a daemon that runs no tailscale node.
func SelfReachDisabled() SelfReach {
	return SelfReach{State: ReachDisabled, Line: "disabled — remote access is off (run 'gmux remote' to enable)"}
}

// SelfReachUnavailable is the state for a node whose LocalAPI did not answer.
// The detail is the read error, kept for the log-visible line.
func SelfReachUnavailable(detail string) SelfReach {
	return SelfReach{State: ReachUnavailable, Line: fmt.Sprintf("NOT reachable — the tailscale node did not answer (%s)", detail)}
}

// SelfReachFromDiag classifies a Diag reading into one status line. `now` is
// injected so the mapping (including the optimistic "expiry already passed"
// case) is testable without a tailscale node.
func SelfReachFromDiag(d DiagStatus, enabled bool, now time.Time) SelfReach {
	if !enabled {
		return SelfReachDisabled()
	}
	if d.StatusError != "" {
		// The read failed: the node may be perfectly fine. Say so instead of
		// inventing a state.
		return SelfReachUnavailable(d.StatusError)
	}
	expired, expiry := selfKeyExpired(d, now)
	base := SelfReach{AuthURL: d.AuthURL, KeyExpiry: d.KeyExpiry}
	hint := reAuthHint
	if d.AuthURL != "" {
		hint = fmt.Sprintf("%s: %s", reAuthHint, d.AuthURL)
	}
	switch {
	case expired:
		base.State = ReachKeyExpired
		if expiry.IsZero() {
			base.Line = fmt.Sprintf("NOT reachable — node key expired; %s", hint)
		} else {
			base.Line = fmt.Sprintf("NOT reachable — node key expired %s; %s", expiry.In(time.UTC).Format(keyExpiryLineFormat), hint)
		}
	case d.BackendState == "NeedsLogin" || d.BackendState == "NoState":
		base.State = ReachNeedsLogin
		base.Line = fmt.Sprintf("NOT reachable — the node needs login (state %s); %s", d.BackendState, hint)
	case d.BackendState == "Running":
		switch {
		// HTTPS first: without certificates in the tailnet, tsnet's
		// ListenTLS fails and the listener never gets its name, so an empty
		// FQDN here is a symptom of the certificate problem, not of a node
		// that is still coming up.
		case !d.HTTPS:
			base.State = ReachNoHTTPS
			base.Line = "NOT reachable — HTTPS is not enabled in this tailnet (enable it at https://login.tailscale.com/admin/dns)"
		case d.FQDN == "":
			base.State = ReachStarting
			base.Line = "starting — the tailnet name is not known yet"
		default:
			base.State = ReachReachable
			base.URL = "https://" + d.FQDN
			base.Line = "reachable at " + base.URL
		}
	case d.BackendState == "Stopped":
		base.State = ReachStopped
		base.Line = fmt.Sprintf("NOT reachable — the tailscale node is stopped; %s", hint)
	default:
		// No backend state yet: the tsnet node has not reported one (it is
		// starting, or, from the caller that has no listener at all, has not
		// been created yet). Say nothing about a state we do not have.
		base.State = ReachStarting
		if d.BackendState == "" {
			base.Line = "starting — the tailscale node is not up yet"
		} else {
			base.Line = fmt.Sprintf("starting — the tailscale node is not up yet (state %s)", d.BackendState)
		}
	}
	return base
}

// SelfReachNoListener classifies a daemon that has no tsnet listener: with
// tailscale enabled the node is still starting (the unix health server starts
// before startup convergence creates the listener, so answering "disabled"
// here would tell the user to enable a setting they already enabled), and
// with it disabled remote access is simply off.
func SelfReachNoListener(enabled bool) SelfReach {
	return SelfReachFromDiag(DiagStatus{}, enabled, time.Time{})
}

// selfKeyExpired reports the node key as expired when tailscale says so, and
// also when the reported expiry has already passed: tailscale sets Expired
// optimistically from the same timestamp, and a status snapshot taken just
// before the deadline must not keep reporting "reachable" after it.
func selfKeyExpired(d DiagStatus, now time.Time) (bool, time.Time) {
	expiry, ok := parseKeyExpiry(d.KeyExpiry)
	if d.Expired {
		return true, expiry
	}
	return ok && !now.Before(expiry), expiry
}

func parseKeyExpiry(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
