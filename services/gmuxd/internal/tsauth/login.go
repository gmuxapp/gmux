package tsauth

import (
	"context"
	"time"

	"tailscale.com/ipn/ipnstate"
)

// Login trigger cadence. requestLogin asks control to start an interactive
// login, then polls the node's status for the AuthURL control publishes, up to
// loginAuthURLTimeout. A responsive control publishes it within a second.
const (
	loginPollInterval   = 250 * time.Millisecond
	loginAuthURLTimeout = 10 * time.Second
)

// loginStatusClient is the LocalAPI seam a re-auth needs: start an interactive
// login, then read the control-provided AuthURL from status.
// *tailscale.LocalClient satisfies it, so production uses the real node and
// tests inject a fake LocalAPI.
type loginStatusClient interface {
	StartLoginInteractive(context.Context) error
	Status(context.Context) (*ipnstate.Status, error)
}

// requestLogin re-authenticates the node c talks to and returns the login URL
// the user must open.
//
// Why the URL is not simply read from status: a node that was previously
// connected and whose key has since expired keeps its FQDN, so the backend does
// not offer a login URL on its own — Status.AuthURL is empty. Asking control to
// start an interactive login (StartLoginInteractive) is what produces a fresh
// URL; the backend then moves to NeedsLogin and publishes AuthURL. This is
// exactly tsnet's own startup path: tsnet runs StartLoginInteractive on a
// NeedsLogin node (tsnet.go:792-794) and its printAuthURLLoop reads the URL
// back from Status (tsnet.go:948-949, "restart with TS_AUTHKEY set, or go
// to: <AuthURL>"), and the tailscale CLI does the same over the IPN bus
// (BrowseToURL, cmd/tailscale/cli/up.go:669).
//
// A node that already publishes a URL (a fresh NeedsLogin node) is returned
// immediately, without triggering a second login. It returns ("", nil) when
// login produced no URL within the window — the caller then falls back to the
// supported restart-with-TS_AUTHKEY step. A StartLoginInteractive error is
// returned so the caller can report a real LocalAPI failure.
func requestLogin(ctx context.Context, c loginStatusClient) (string, error) {
	if st, err := c.Status(ctx); err == nil && st != nil && st.AuthURL != "" {
		return st.AuthURL, nil
	}
	if err := c.StartLoginInteractive(ctx); err != nil {
		return "", err
	}
	deadline := time.Now().Add(loginAuthURLTimeout)
	for {
		st, err := c.Status(ctx)
		if err == nil && st != nil && st.AuthURL != "" {
			return st.AuthURL, nil
		}
		if time.Now().After(deadline) {
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

// RequestLogin re-authenticates gmuxd's own tailscale node and returns the
// login URL to open. It is the daemon-side half of `gmux remote`'s recovery for
// an expired node key: the CLI is a separate process with no LocalClient, so
// the login has to be triggered in-process by the node's owner. It returns
// ("", nil) when there is no tsnet node at all — the caller treats that as "no
// URL, print the fallback".
func (l *Listener) RequestLogin(ctx context.Context) (string, error) {
	if l.lc == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, loginAuthURLTimeout+2*time.Second)
	defer cancel()
	return requestLogin(ctx, l.lc)
}
