package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
)

// mockLoginNode is a fake tsnet Listener for the login endpoint: it reports a
// fixed Diag and counts whether a login was triggered. A real key expiry cannot
// be produced locally, so the states are supplied exactly as Diag would read
// them from the LocalAPI.
type mockLoginNode struct {
	mu      sync.Mutex
	diag    tsauth.DiagStatus
	authURL string
	err     error
	calls   int
}

func (m *mockLoginNode) Diag() tsauth.DiagStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diag
}

func (m *mockLoginNode) RequestLogin(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.authURL, m.err
}

func (m *mockLoginNode) loginCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func postLogin(t *testing.T, node remoteLoginNode) *httptest.ResponseRecorder {
	t.Helper()
	return postLoginWithGetter(t, func() remoteLoginNode { return node })
}

func postLoginWithGetter(t *testing.T, get func() remoteLoginNode) *httptest.ResponseRecorder {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/remote/login", nil)
	remoteLoginHandler(get, now)(rec, req)
	return rec
}

func decodeLoginBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// A healthy node must be REFUSED with 409 and its current state+line, and the
// login must never be triggered: starting an interactive login cancels a live
// session, so a stray local POST must not touch a node that is working.
func TestRemoteLoginHandler_HealthyRefused(t *testing.T) {
	node := &mockLoginNode{
		diag: tsauth.DiagStatus{
			FQDN: "gmux.tailnet.ts.net", HTTPS: true, MagicDNS: true,
			BackendState: "Running", Connected: true,
		},
		authURL: "https://login.tailscale.com/a/should-not-be-used",
	}
	rec := postLogin(t, node)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if got := node.loginCalls(); got != 0 {
		t.Errorf("RequestLogin calls = %d, want 0 on a healthy node", got)
	}
	body := decodeLoginBody(t, rec)
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("healthy refusal must be ok=false, got %v", body["ok"])
	}
	msg := ""
	if e, ok := body["error"].(map[string]any); ok {
		msg, _ = e["message"].(string)
	}
	// The short error carries the current state and the status line.
	if !strings.Contains(msg, tsauth.ReachReachable) || !strings.Contains(msg, "reachable at https://gmux.tailnet.ts.net") {
		t.Errorf("error message %q must carry the state and line", msg)
	}
}

// The transient/unknown states are not re-auth states either: they must be
// refused without triggering a login (the CLI never asks for them).
func TestRemoteLoginHandler_NonLoginStatesRefused(t *testing.T) {
	for name, d := range map[string]tsauth.DiagStatus{
		"unavailable": {FQDN: "gmux.tailnet.ts.net", StatusError: "context deadline exceeded"},
		"starting":    {BackendState: "Starting"},
		"https off":   {BackendState: "Running", FQDN: "gmux.tailnet.ts.net"},
	} {
		t.Run(name, func(t *testing.T) {
			node := &mockLoginNode{diag: d}
			rec := postLogin(t, node)
			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
			}
			if got := node.loginCalls(); got != 0 {
				t.Errorf("RequestLogin calls = %d, want 0", got)
			}
		})
	}
}

// A node that needs login proceeds: the login is triggered and the URL returned.
func TestRemoteLoginHandler_NeedsLoginProceeds(t *testing.T) {
	expired := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	cases := map[string]tsauth.DiagStatus{
		"needs_login": {BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/x"},
		// Expired key that keeps its FQDN and even says Connected, the exact
		// leftover state the guard must still treat as needs-login.
		"key_expired": {FQDN: "gmux.tailnet.ts.net", HTTPS: true, BackendState: "Running", Connected: true, Expired: true, KeyExpiry: expired},
		"stopped":     {BackendState: "Stopped"},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			node := &mockLoginNode{diag: d, authURL: "https://login.tailscale.com/a/fresh"}
			rec := postLogin(t, node)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body)
			}
			if got := node.loginCalls(); got != 1 {
				t.Errorf("RequestLogin calls = %d, want 1", got)
			}
			body := decodeLoginBody(t, rec)
			data, _ := body["data"].(map[string]any)
			if url, _ := data["auth_url"].(string); url != "https://login.tailscale.com/a/fresh" {
				t.Errorf("auth_url = %q, want the login URL", url)
			}
		})
	}
}

// Regression (Greptile on #549): the unix mux registers POST /v1/remote/login
// BEFORE startup convergence creates the tsnet listener. The handler must
// therefore resolve the node at REQUEST time, not capture it at registration —
// otherwise every request sees the nil captured at registration and an expired
// node can never trigger a login (and a healthy node never reaches the 409).
func TestRemoteLoginHandler_ListenerInitializedAfterRegistration(t *testing.T) {
	var mu sync.Mutex
	var node remoteLoginNode // nil when the route is registered, like production
	get := func() remoteLoginNode {
		mu.Lock()
		defer mu.Unlock()
		return node
	}
	set := func(n remoteLoginNode) {
		mu.Lock()
		node = n
		mu.Unlock()
	}

	// Register ONCE while the node is still nil (as production does), then
	// reuse that same handler after convergence stores the node. This is the
	// regression: a handler that captured the node at registration stays nil
	// forever; resolving per request sees the late-installed node.
	now := func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	h := remoteLoginHandler(get, now)
	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/v1/remote/login", nil))
		return rec
	}

	// Before convergence: no node, so the endpoint reports no URL.
	rec := do()
	if rec.Code != http.StatusOK {
		t.Errorf("before init: status = %d, want 200", rec.Code)
	}

	// After convergence the SAME handler must see the node: an expired key now
	// triggers a login and returns its URL.
	expired := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	expiredNode := &mockLoginNode{
		diag:    tsauth.DiagStatus{FQDN: "gmux.tailnet.ts.net", HTTPS: true, BackendState: "Running", Connected: true, Expired: true, KeyExpiry: expired},
		authURL: "https://login.tailscale.com/a/late",
	}
	set(expiredNode)
	rec = do()
	if rec.Code != http.StatusOK {
		t.Errorf("expired after init: status = %d, want 200", rec.Code)
	}
	if got := expiredNode.loginCalls(); got != 1 {
		t.Errorf("expired after init: RequestLogin calls = %d, want 1", got)
	}
	body := decodeLoginBody(t, rec)
	if data, _ := body["data"].(map[string]any); data["auth_url"] != "https://login.tailscale.com/a/late" {
		t.Errorf("auth_url = %v, want the login URL", body["data"])
	}

	// A healthy node installed later must be refused by the guard, proving the
	// late-resolved node reaches the guard too.
	healthy := &mockLoginNode{diag: tsauth.DiagStatus{FQDN: "gmux.tailnet.ts.net", HTTPS: true, MagicDNS: true, BackendState: "Running", Connected: true}}
	set(healthy)
	rec = do()
	if rec.Code != http.StatusConflict {
		t.Errorf("healthy after init: status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if got := healthy.loginCalls(); got != 0 {
		t.Errorf("healthy after init: RequestLogin calls = %d, want 0", got)
	}
}

// No tsnet node: nothing to log in. Report an empty URL (the CLI's supported
// fallback) without an error.
func TestRemoteLoginHandler_NoNode(t *testing.T) {
	rec := postLogin(t, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := decodeLoginBody(t, rec)
	data, _ := body["data"].(map[string]any)
	if url, ok := data["auth_url"].(string); !ok || url != "" {
		t.Errorf("auth_url = %v, want \"\"", data["auth_url"])
	}
}
