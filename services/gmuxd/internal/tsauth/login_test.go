package tsauth

import (
	"context"
	"errors"
	"sync"
	"testing"

	"tailscale.com/ipn/ipnstate"
)

// mockLocalAPI is a fake tsnet LocalAPI for the re-auth path. It models a node
// that was previously connected (it keeps its FQDN) whose key has expired: it
// publishes no AuthURL until an interactive login is started, at which point
// control supplies one. A real node-key expiry cannot be produced in a unit
// test, so the transition is driven exactly as tailscale's own CLI does it —
// StartLoginInteractive then Status.
type mockLocalAPI struct {
	mu sync.Mutex

	// authURL is what Status reports; it is filled after a login is started.
	authURL       string
	statusErr     error
	startErr      error
	startCalls    int
	statusCalls   int
	startLoginURL string // URL that appears once a login is triggered
}

func (m *mockLocalAPI) StartLoginInteractive(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startCalls++
	if m.startErr != nil {
		return m.startErr
	}
	// Starting a login is what makes control publish the URL.
	if m.authURL == "" {
		m.authURL = m.startLoginURL
	}
	return nil
}

func (m *mockLocalAPI) Status(context.Context) (*ipnstate.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCalls++
	if m.statusErr != nil {
		return nil, m.statusErr
	}
	return &ipnstate.Status{
		BackendState: "Running", // a previously-running node, key expired
		AuthURL:      m.authURL,
		Self:         &ipnstate.PeerStatus{DNSName: "gmux.tailnet.ts.net.", Expired: true},
	}, nil
}

// An expired key on a previously-connected node publishes no login URL, so the
// login must be triggered: StartLoginInteractive runs and the URL control
// supplies is returned.
func TestRequestLogin_ExpiredKeyTriggersLogin(t *testing.T) {
	const url = "https://login.tailscale.com/a/expired"
	m := &mockLocalAPI{startLoginURL: url} // authURL empty until login starts
	got, err := requestLogin(context.Background(), m)
	if err != nil {
		t.Fatalf("requestLogin: %v", err)
	}
	if got != url {
		t.Fatalf("AuthURL = %q, want %q", got, url)
	}
	if m.startCalls != 1 {
		t.Fatalf("StartLoginInteractive calls = %d, want 1", m.startCalls)
	}
}

// A node that already publishes a login URL (e.g. a fresh NeedsLogin node) is
// returned as-is, without triggering a redundant interactive login.
func TestRequestLogin_UsesExistingAuthURL(t *testing.T) {
	const url = "https://login.tailscale.com/a/already"
	m := &mockLocalAPI{authURL: url}
	got, err := requestLogin(context.Background(), m)
	if err != nil {
		t.Fatalf("requestLogin: %v", err)
	}
	if got != url {
		t.Fatalf("AuthURL = %q, want %q", got, url)
	}
	if m.startCalls != 0 {
		t.Fatalf("StartLoginInteractive must not be called when a URL exists, calls=%d", m.startCalls)
	}
}

// A LocalAPI that refuses the login surfaces the error to the caller, which
// reports it instead of inventing a URL.
func TestRequestLogin_StartErrorPropagates(t *testing.T) {
	m := &mockLocalAPI{startErr: errors.New("localapi: not permitted")}
	got, err := requestLogin(context.Background(), m)
	if err == nil || !errors.Is(err, m.startErr) {
		t.Fatalf("err = %v, want %v", err, m.startErr)
	}
	if got != "" {
		t.Fatalf("AuthURL = %q, want empty on login failure", got)
	}
}

// No listener (no LocalClient): nothing to re-auth, so RequestLogin reports
// "no URL, no error" and the caller prints the supported fallback.
func TestListenerRequestLogin_NoClient(t *testing.T) {
	l := &Listener{} // lc is nil
	got, err := l.RequestLogin(context.Background())
	if err != nil {
		t.Fatalf("RequestLogin: %v", err)
	}
	if got != "" {
		t.Fatalf("AuthURL = %q, want empty when there is no node", got)
	}
}
