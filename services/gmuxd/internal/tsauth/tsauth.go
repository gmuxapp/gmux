// Package tsauth provides an optional tailscale (tsnet) HTTPS listener
// with identity-based access control.
//
// When enabled, gmuxd joins the user's tailnet and serves the same HTTP
// handler as the localhost listener, but wrapped in middleware that:
//  1. Enforces HTTPS (tsnet provides automatic Let's Encrypt certs).
//  2. Checks the connecting peer's tailscale identity (via WhoIs) against
//     an allow list of login names.
//
// The node owner's tailscale account is automatically added to the allow
// list at startup. Additional users can be added via config. Peers not
// on the list are rejected with 403.
package tsauth

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

// Config mirrors the tailscale section of the gmuxd config file.
type Config struct {
	Hostname string
	Allow    []string // tailscale login names (e.g. "user@github")
}

// DiagStatus contains diagnostic information about the Tailscale connection.
type DiagStatus struct {
	// FQDN is the full tailnet DNS name (e.g. "gmux.tailnet.ts.net").
	// Empty if not yet connected.
	FQDN string `json:"fqdn,omitempty"`
	// MagicDNS indicates whether MagicDNS is enabled in the tailnet.
	// Without it, the FQDN won't resolve from other devices.
	MagicDNS bool `json:"magic_dns"`
	// HTTPS indicates whether HTTPS certificates are available.
	// Without it, the browser will refuse to connect.
	HTTPS bool `json:"https"`
	// AuthURL is set when the node needs login. The user must visit
	// this URL to register the device in their tailnet.
	AuthURL string `json:"auth_url,omitempty"`
	// Connected is true when the listener is fully operational.
	Connected bool `json:"connected"`
}

// Listener manages a tsnet server and its HTTPS listener.
type Listener struct {
	srv   *tsnet.Server
	lc    *tailscale.LocalClient
	cfg   Config
	fqdn  string         // resolved tailnet FQDN, set once ready
	ready chan struct{}   // closed when the listener is fully connected
}

// FQDN returns the full tailnet DNS name (e.g. "gmuxd.angler-map.ts.net")
// once the listener is ready. Returns "" if tailscale hasn't connected yet.
func (l *Listener) FQDN() string { return l.fqdn }

// Diag returns diagnostic status about the Tailscale connection.
// Safe to call at any time, including before the listener is ready.
func (l *Listener) Diag() DiagStatus {
	ds := DiagStatus{
		FQDN:      l.fqdn,
		Connected: l.fqdn != "",
	}
	if l.lc == nil {
		return ds
	}
	status, err := l.lc.Status(context.Background())
	if err != nil {
		return ds
	}
	if status.AuthURL != "" {
		ds.AuthURL = status.AuthURL
	}
	ds.HTTPS = len(status.CertDomains) > 0
	if status.CurrentTailnet != nil {
		ds.MagicDNS = status.CurrentTailnet.MagicDNSSuffix != ""
	} else {
		ds.MagicDNS = status.MagicDNSSuffix != ""
	}
	return ds
}

// Start joins the tailnet and begins serving handler over HTTPS on :443.
// The tailscale login and listener startup happen in the background so
// the caller (main) can proceed to start the localhost listener immediately.
// Call Shutdown to stop.
func Start(cfg Config, stateDir string, handler http.Handler) *Listener {
	srv := &tsnet.Server{
		Hostname: cfg.Hostname,
		Dir:      filepath.Join(stateDir, "tsnet"),
	}

	l := &Listener{
		srv:   srv,
		cfg:   cfg,
		ready: make(chan struct{}),
	}

	go l.run(handler)
	return l
}

// Ready returns a channel that is closed once the tailscale listener is
// fully connected and serving. Callers that depend on LocalClient or
// FQDN should select on this before proceeding.
func (l *Listener) Ready() <-chan struct{} { return l.ready }

// LocalClient returns the tailscale LocalClient for API calls such as
// Status(). Only valid after Ready() is closed.
func (l *Listener) LocalClient() *tailscale.LocalClient { return l.lc }

// Transport returns an http.RoundTripper that routes through the tsnet
// server's WireGuard tunnel. Use this for HTTP clients that need to
// reach other tailnet devices.
func (l *Listener) Transport() http.RoundTripper {
	return &http.Transport{DialContext: l.srv.Dial}
}

// run does the blocking tailscale startup in a background goroutine.
func (l *Listener) run(handler http.Handler) {
	if err := l.srv.Start(); err != nil {
		log.Printf("tsauth: tsnet start: %v", err)
		return
	}

	lc, err := l.srv.LocalClient()
	if err != nil {
		log.Printf("tsauth: local client: %v", err)
		return
	}
	l.lc = lc

	// Wait for the node to be authenticated. On first run, the user must
	// visit the auth URL printed in the logs.
	ownerLogin, err := resolveOwnerLogin(lc)
	if err != nil {
		log.Printf("tsauth: could not determine node owner: %v", err)
		return
	}
	l.cfg.Allow = addIfMissing(l.cfg.Allow, ownerLogin)
	log.Printf("tsauth: node owner %s auto-whitelisted", ownerLogin)

	// HTTPS listener with automatic certs from tailscale.
	ln, err := l.srv.ListenTLS("tcp", ":443")
	if err != nil {
		log.Printf("tsauth: listen TLS: %v", err)
		return
	}

	// Resolve the full tailnet FQDN so users know exactly what to type.
	fqdn := l.cfg.Hostname
	if status, err := lc.Status(context.Background()); err == nil && status.Self != nil {
		if dnsName := strings.TrimSuffix(status.Self.DNSName, "."); dnsName != "" {
			fqdn = dnsName
		}
	}
	l.fqdn = fqdn
	close(l.ready)
	log.Printf("tsauth: connected")

	authed := l.authMiddleware(handler)
	if err := http.Serve(ln, authed); err != nil {
		log.Printf("tsauth: serve: %v", err)
	}
}

// Shutdown stops the tsnet server.
func (l *Listener) Shutdown() {
	l.srv.Close()
}

// authMiddleware wraps a handler with tailscale identity checks.
func (l *Listener) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := l.lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			log.Printf("tsauth: WhoIs(%s): %v", r.RemoteAddr, err)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		loginName := who.UserProfile.LoginName

		if !l.isAllowed(loginName) {
			log.Printf("tsauth: DENIED %s (login=%s device=%s)", r.RemoteAddr, loginName, who.Node.ComputedName)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isAllowed checks if the connecting peer's login name matches any entry
// in the allow list. Login names (e.g. "user@github") are stable identities
// tied to the user's auth provider. Device names are not checked — use
// tailscale ACLs for per-device control.
// Comparison is case-insensitive.
func (l *Listener) isAllowed(loginName string) bool {
	if loginName == "" {
		return false
	}
	loginLower := strings.ToLower(loginName)

	for _, entry := range l.cfg.Allow {
		if strings.ToLower(entry) == loginLower {
			return true
		}
	}
	return false
}

// resolveOwnerLogin waits for the tsnet node to be authenticated, then
// returns the login name of the node owner. On first run, the user must
// complete the tailscale login flow — check the logs for the auth URL.
func resolveOwnerLogin(lc *tailscale.LocalClient) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	prompted := false
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		status, err := lc.Status(ctx)
		if err != nil {
			return "", fmt.Errorf("status: %w", err)
		}

		// If NeedsLogin, tell the user once and keep waiting.
		if status.BackendState == "NeedsLogin" || status.BackendState == "NoState" {
			if !prompted {
				if status.AuthURL != "" {
					log.Printf("tsauth: tailscale needs login — visit: %s", status.AuthURL)
				} else {
					log.Printf("tsauth: waiting for tailscale login...")
				}
				prompted = true
			}
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("timed out waiting for tailscale login (state: %s)", status.BackendState)
			case <-tick.C:
				continue
			}
		}

		if status.Self == nil {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("no self node in status (state: %s)", status.BackendState)
			case <-tick.C:
				continue
			}
		}

		profile, ok := status.User[status.Self.UserID]
		if !ok || profile.LoginName == "" {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("no user profile for UserID %d (state: %s)", status.Self.UserID, status.BackendState)
			case <-tick.C:
				continue
			}
		}

		return profile.LoginName, nil
	}
}

// addIfMissing appends entry to the list if not already present (case-insensitive).
func addIfMissing(list []string, entry string) []string {
	entryLower := strings.ToLower(entry)
	for _, existing := range list {
		if strings.ToLower(existing) == entryLower {
			return list
		}
	}
	return append(list, entry)
}
