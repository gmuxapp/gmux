package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
	"tailscale.com/ipn/ipnstate"
)

type retryPeerClient struct {
	mu    sync.Mutex
	calls int
}

func (c *retryPeerClient) Status(context.Context) (*ipnstate.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 {
		return nil, errors.New("transient status failure")
	}
	return &ipnstate.Status{MagicDNSSuffix: "tailnet.ts.net."}, nil
}

func (*retryPeerClient) DialTCP(context.Context, string, uint16) (net.Conn, error) {
	return nil, errors.New("not used")
}

type markerTransport struct{}

func (markerTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Route": []string{"tsnet"}}, Body: http.NoBody}, nil
}

func TestConvergeTailnetPeerTransportRetriesSuffix(t *testing.T) {
	transport := tsauth.NewRoutedTransport()
	client := &retryPeerClient{}
	retry := make(chan time.Time, 2)
	reconnected := make(chan struct{}, 2)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		convergeTailnetPeerTransport(ctx, transport, "", markerTransport{}, client, func() { reconnected <- struct{}{} }, retry)
		close(done)
	}()
	<-reconnected // embedded client installed immediately despite empty suffix
	retry <- time.Now()
	retry <- time.Now()
	<-reconnected // suffix converged after transient failure

	req, _ := http.NewRequest(http.MethodGet, "https://host.tailnet.ts.net/v1/health", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil || resp.Header.Get("X-Route") != "tsnet" {
		t.Fatalf("same-tailnet route did not converge: resp=%v err=%v", resp, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("convergence did not finish")
	}
}

// probePeerHealth is the contract the add-peer flow relies on: it must
// reach /v1/health, forward the bearer token, and pull node_id + name
// out of the {data:{…}} envelope — and fail clearly when the host is
// unreachable/unauthorized or reports no name.
func TestProbePeerHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":{"service":"gmuxd","node_id":"node_abc","hostname":"gmux-laptop"}}`))
	}))
	defer srv.Close()

	id, name, err := probePeerHealth(context.Background(), http.DefaultTransport, srv.URL, "tok")
	if err != nil {
		t.Fatalf("probePeerHealth: %v", err)
	}
	if id != "node_abc" || name != "gmux-laptop" {
		t.Fatalf("got (node_id=%q, name=%q), want (node_abc, gmux-laptop)", id, name)
	}
}

func TestProbePeerHealth_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, _, err := probePeerHealth(context.Background(), http.DefaultTransport, srv.URL, ""); err == nil {
		t.Fatal("expected an error when the probe is unauthorized")
	}
}

func TestProbePeerHealth_MissingName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"service":"gmuxd","node_id":"node_abc"}}`))
	}))
	defer srv.Close()
	// A host that reports no name can't be given a routing identity.
	if _, _, err := probePeerHealth(context.Background(), http.DefaultTransport, srv.URL, ""); err == nil {
		t.Fatal("expected an error when the host reports no name")
	}
}

func TestProbePeerHealth_NotGmux(t *testing.T) {
	// A reachable HTTP endpoint that isn't gmuxd must be rejected, so a
	// stray URL can't be registered as a peer (parity with discovery).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"service":"other","hostname":"x"}}`))
	}))
	defer srv.Close()
	if _, _, err := probePeerHealth(context.Background(), http.DefaultTransport, srv.URL, ""); err == nil {
		t.Fatal("expected an error when the host is not running gmux")
	}
}
