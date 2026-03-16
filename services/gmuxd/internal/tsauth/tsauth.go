// Package tsauth provides an optional tailscale (tsnet) HTTPS listener
// with identity-based access control.
//
// When enabled, gmuxd joins the user's tailnet and serves the same HTTP
// handler as the localhost listener, but wrapped in middleware that:
//  1. Enforces HTTPS (tsnet provides automatic Let's Encrypt certs).
//  2. Checks the connecting peer's tailscale identity (via WhoIs) against
//     a configured allow list of login names and/or device names.
//
// The allow list is fail-closed: if empty, all connections are rejected.
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

// Listener manages a tsnet server and its HTTPS listener.
type Listener struct {
	srv *tsnet.Server
	lc  *tailscale.LocalClient
	cfg Config
}

// Start joins the tailnet and begins serving handler over HTTPS on :443.
// It blocks in a goroutine — call Shutdown to stop.
func Start(cfg Config, stateDir string, handler http.Handler) (*Listener, error) {
	srv := &tsnet.Server{
		Hostname: cfg.Hostname,
		Dir:      filepath.Join(stateDir, "tsnet"),
	}

	// Start the tsnet node and wait for it to be ready.
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("tsauth: tsnet start: %w", err)
	}

	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsauth: local client: %w", err)
	}

	// Auto-whitelist the node owner's tailscale account.
	ownerLogin, err := resolveOwnerLogin(lc)
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsauth: could not determine node owner: %w", err)
	}
	cfg.Allow = addIfMissing(cfg.Allow, ownerLogin)
	log.Printf("tsauth: node owner %s auto-whitelisted", ownerLogin)

	l := &Listener{
		srv: srv,
		lc:  lc,
		cfg: cfg,
	}

	// HTTPS listener with automatic certs from tailscale.
	ln, err := srv.ListenTLS("tcp", ":443")
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsauth: listen TLS: %w", err)
	}

	go func() {
		authed := l.authMiddleware(handler)
		if err := http.Serve(ln, authed); err != nil {
			log.Printf("tsauth: serve: %v", err)
		}
	}()

	log.Printf("tsauth: listening on https://%s (allowed: %v)", cfg.Hostname, cfg.Allow)
	return l, nil
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

// resolveOwnerLogin fetches the tailscale status and returns the login name
// of the user who owns this node.
func resolveOwnerLogin(lc *tailscale.LocalClient) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status, err := lc.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	if status.Self == nil {
		return "", fmt.Errorf("no self node in status")
	}

	// status.Self.UserID maps to a profile in status.User.
	profile, ok := status.User[status.Self.UserID]
	if !ok {
		return "", fmt.Errorf("no user profile for UserID %d", status.Self.UserID)
	}
	if profile.LoginName == "" {
		return "", fmt.Errorf("empty login name for UserID %d", status.Self.UserID)
	}

	return profile.LoginName, nil
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
