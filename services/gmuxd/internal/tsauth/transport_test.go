package tsauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

type stubRT struct{ name string }

func (s *stubRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{Header: http.Header{"X-Stub": []string{s.name}}}, nil
}

func reqFor(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return &http.Request{URL: u}
}

// Before embedded tsnet is ready, foreign .ts.net names enter the foreign
// transport but its nil client delegates to ordinary DNS/proxy behavior.
func TestRoutedTransport_RoutingBeforeEmbeddedReady(t *testing.T) {
	fallback := &stubRT{name: "fallback"}
	foreign := &stubRT{name: "foreign"}
	rt := &RoutedTransport{fallback: fallback, foreign: foreign}

	for _, tc := range []struct {
		url  string
		want http.RoundTripper
	}{
		{"https://gmux.tailnet.ts.net", foreign},
		{"http://192.168.1.10:9999", fallback},
		{"http://localhost:9999", fallback},
	} {
		if got := rt.pick(reqFor(t, tc.url).URL.Hostname()); got != tc.want {
			t.Errorf("pick(%s) before SetTailnet = %T, want %T", tc.url, got, tc.want)
		}
	}
}

// After SetTailnet, local-suffix names use embedded tsnet, foreign .ts.net
// names use exact LocalAPI membership, and ordinary hosts keep the fallback.
func TestRoutedTransport_SuffixRouting(t *testing.T) {
	fallback := &stubRT{name: "fallback"}
	foreign := &stubRT{name: "foreign"}
	ts := &stubRT{name: "tsnet"}
	rt := &RoutedTransport{fallback: fallback, foreign: foreign}
	rt.SetTailnet("tailnet.ts.net", ts, nil)

	cases := []struct {
		url  string
		want http.RoundTripper
	}{
		{"https://gmux.tailnet.ts.net", ts},
		{"https://gmux.tailnet.ts.net:443", ts},
		{"https://GMUX.TAILNET.TS.NET", ts},
		{"https://gmux.tailnet.ts.net./v1", ts},
		{"https://other.othernet.ts.net", foreign},
		{"https://evil-tailnet.ts.net", foreign},
		{"http://192.168.1.10:9999", fallback},
		{"http://localhost:9999", fallback},
		{"http://nas.lan:9999", fallback},
	}
	for _, c := range cases {
		if got := rt.pick(reqFor(t, c.url).URL.Hostname()); got != c.want {
			t.Errorf("pick(%s) = %T, want %T", c.url, got, c.want)
		}
	}
}

// RoundTrip must dispatch to whichever transport pick selects — the
// routing decision is worthless if the actual request path ignores it.
func TestRoutedTransport_RoundTripDispatch(t *testing.T) {
	rt := &RoutedTransport{fallback: &stubRT{name: "fallback"}, foreign: &stubRT{name: "foreign"}}
	rt.SetTailnet("tailnet.ts.net", &stubRT{name: "tsnet"}, nil)

	for _, c := range []struct{ url, want string }{
		{"https://gmux.tailnet.ts.net/v1/health", "tsnet"},
		{"https://gmux.othernet.ts.net/v1/health", "foreign"},
		{"http://192.168.1.10:9999/v1/health", "fallback"},
	} {
		resp, err := rt.RoundTrip(reqFor(t, c.url))
		if err != nil {
			t.Fatalf("RoundTrip(%s): %v", c.url, err)
		}
		if got := resp.Header.Get("X-Stub"); got != c.want {
			t.Errorf("RoundTrip(%s) went through %q, want %q", c.url, got, c.want)
		}
	}
}

// SetTailnet normalizes the suffix so a trailing-dot value from
// tailscale's Status (e.g. "tailnet.ts.net.") still matches.
func TestRoutedTransport_NormalizesSuffix(t *testing.T) {
	ts := &stubRT{name: "tsnet"}
	rt := NewRoutedTransport()
	rt.SetTailnet("Tailnet.ts.net.", ts, nil)

	if got := rt.pick("gmux.tailnet.ts.net"); got != ts {
		t.Errorf("suffix with trailing dot/case: got fallback, want tsnet")
	}
}

type fakeStatusClient struct {
	mu         sync.Mutex
	status     *ipnstate.Status
	err        error
	calls      int
	dial       contextDialer
	statusFunc func(context.Context) (*ipnstate.Status, error)
}

func (f *fakeStatusClient) Status(ctx context.Context) (*ipnstate.Status, error) {
	f.mu.Lock()
	f.calls++
	fn, status, err := f.statusFunc, f.status, f.err
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return status, err
}

func (f *fakeStatusClient) DialTCP(ctx context.Context, host string, port uint16) (net.Conn, error) {
	f.mu.Lock()
	dial := f.dial
	f.mu.Unlock()
	if dial == nil {
		return nil, errors.New("unexpected LocalAPI dial")
	}
	return dial.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
}

func peerStatus(entries ...*ipnstate.PeerStatus) *ipnstate.Status {
	st := &ipnstate.Status{Peer: make(map[key.NodePublic]*ipnstate.PeerStatus)}
	for _, p := range entries {
		st.Peer[key.NewNode().Public()] = p
	}
	return st
}

type recordingDialer struct {
	mu        sync.Mutex
	addresses []string
	err       error
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addresses = append(d.addresses, address)
	d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestForeignPeerExactMembershipAndNormalization(t *testing.T) {
	member := &ipnstate.PeerStatus{DNSName: "FT.Tail95157A.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.8")}}
	status := peerStatus(member)
	if got := exactPeers(status, "ft.tail95157a.ts.net..."); len(got) != 1 || got[0] != member {
		t.Fatalf("normalized exact match = %v", got)
	}
	// Mutation guard: suffix/substring membership would incorrectly match
	// this populated-map near miss.
	if got := exactPeers(status, "tail95157a.ts.net"); len(got) != 0 {
		t.Fatalf("near-miss name matched peer: %v", got)
	}
}

func TestForeignPeerMapRefresh(t *testing.T) {
	status := &fakeStatusClient{status: peerStatus(&ipnstate.PeerStatus{DNSName: "ft.foreign.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.8")}})}
	var selected [][]netip.Addr
	exact := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		target := r.Context().Value(peerAddressesKey{}).(peerDialTarget)
		selected = append(selected, append([]netip.Addr(nil), target.ips...))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})
	now := time.Unix(100, 0)
	foreign := &foreignPeerTransport{client: status, exact: exact, fallback: &stubRT{name: "fallback"}, now: func() time.Time { return now }}
	req := reqFor(t, "https://ft.foreign.ts.net/v1/health")
	if _, err := foreign.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	status.mu.Lock()
	status.status = peerStatus(&ipnstate.PeerStatus{DNSName: "ft.foreign.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}})
	status.mu.Unlock()
	now = now.Add(peerStatusFreshFor + time.Millisecond)
	if _, err := foreign.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0][0].String() != "100.64.0.8" || selected[1][0].String() != "100.64.0.9" {
		t.Fatalf("selected maps = %v", selected)
	}
}

func TestForeignPeerSecurityAndFailureModes(t *testing.T) {
	cases := []struct {
		name               string
		status             *ipnstate.Status
		statusErr          error
		wantRoute, wantErr string
	}{
		{name: "unknown falls back", status: peerStatus(), wantRoute: "fallback"},
		{name: "LocalAPI unavailable fails closed", statusErr: errors.New("no localapi"), wantErr: "status unavailable"},
		{name: "offline rejects stale IP", status: peerStatus(&ipnstate.PeerStatus{DNSName: "ft.foreign.ts.net.", Online: false, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.8")}}), wantErr: "offline"},
		{name: "duplicate DNS rejects", status: peerStatus(
			&ipnstate.PeerStatus{DNSName: "ft.foreign.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.8")}},
			&ipnstate.PeerStatus{DNSName: "FT.FOREIGN.TS.NET", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}}), wantErr: "ambiguous"},
		{name: "non-Tailscale address rejects", status: peerStatus(&ipnstate.PeerStatus{DNSName: "ft.foreign.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}), wantErr: "no dialable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exactCalled := false
			foreign := &foreignPeerTransport{
				client:   &fakeStatusClient{status: tc.status, err: tc.statusErr},
				fallback: &stubRT{name: "fallback"},
				exact: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					exactCalled = true
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
				}),
			}
			resp, err := foreign.RoundTrip(reqFor(t, "https://ft.foreign.ts.net/v1/health"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				if exactCalled {
					t.Fatal("rejected peer reached exact transport")
				}
				return
			}
			if err != nil || resp.Header.Get("X-Stub") != tc.wantRoute || exactCalled {
				t.Fatalf("resp=%v err=%v exact=%v", resp, err, exactCalled)
			}
		})
	}
}

func TestDialAnyRacesIPv6AndIPv4(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	dialer := dialFunc(func(ctx context.Context, _, address string) (net.Conn, error) {
		started <- address
		if strings.HasPrefix(address, "[") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		<-release
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, err := dialAny(ctx, dialer, "tcp", "443", []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::8"), netip.MustParseAddr("100.64.0.8")})
		done <- result{c, err}
	}()
	got := []string{<-started, <-started}
	close(release)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	_ = r.conn.Close()
	if len(got) != 2 {
		t.Fatalf("tried %v, want both families", got)
	}
}

func TestRoutedTransportForeignPeerPreservesTLSHostnameAndSNI(t *testing.T) {
	const host = "ft.tail95157a.ts.net"
	cert, roots := dnsCertificate(t, host)
	var gotSNI string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
		gotSNI = hi.ServerName
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()
	serverAddr := srv.Listener.Addr().String()

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	status := &fakeStatusClient{status: peerStatus(&ipnstate.PeerStatus{DNSName: host + ".", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.8")}})}
	dial := dialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverAddr)
	})
	status.dial = dial
	rt := newRoutedTransport(base, status, dial)
	resp, err := (&http.Client{Transport: rt}).Get("https://" + host + "/v1/health")
	if err != nil {
		t.Fatalf("verified request: %v", err)
	}
	_ = resp.Body.Close()
	if gotSNI != host {
		t.Fatalf("SNI=%q, want %q", gotSNI, host)
	}

	// Mutation probe: removing the DNS SAN / changing the original hostname
	// must fail verification. This proves routing did not silently disable TLS.
	_, err = (&http.Client{Transport: rt}).Get("https://wrong.tail95157a.ts.net/v1/health")
	if err == nil {
		t.Fatal("wrong TLS hostname unexpectedly verified")
	}
}

type dialFunc func(context.Context, string, string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func dnsCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return cert, roots
}

func TestForeignExactPeerBypassesProxyButUnknownKeepsFallback(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy.invalid:8080") }
	rt := newRoutedTransport(base, &fakeStatusClient{status: peerStatus()}, &net.Dialer{})
	foreign, ok := rt.foreign.(*foreignPeerTransport)
	if !ok {
		t.Fatalf("foreign transport type = %T", rt.foreign)
	}
	exact, ok := foreign.exact.(*http.Transport)
	if !ok || exact.Proxy != nil {
		t.Fatal("exact netmap route did not bypass proxy")
	}
	if foreign.fallback != base || base.Proxy == nil {
		t.Fatal("unknown-name fallback lost proxy configuration")
	}
}

func TestSetTailnetSwitchesForeignMembershipAndDialToEmbeddedClient(t *testing.T) {
	const foreignHost = "ft.tail95157a.ts.net"
	system := &fakeStatusClient{status: peerStatus()}
	embedded := &fakeStatusClient{status: peerStatus(&ipnstate.PeerStatus{
		DNSName: foreignHost + ".", Online: true,
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.125.29.31")},
	})}
	base := http.DefaultTransport.(*http.Transport).Clone()
	rt := newRoutedTransport(base, system, &net.Dialer{})
	foreign := rt.foreign.(*foreignPeerTransport)
	foreign.fallback = &stubRT{name: "fallback"}
	foreign.exact = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		target := r.Context().Value(peerAddressesKey{}).(peerDialTarget)
		if target.client != embedded || len(target.ips) != 1 || target.ips[0].String() != "100.125.29.31" {
			t.Fatalf("dial target = %#v", target)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Stub": []string{"embedded"}}, Body: http.NoBody, Request: r}, nil
	})

	req := reqFor(t, "https://"+foreignHost+"/v1/health")
	resp, err := rt.RoundTrip(req)
	if err != nil || resp.Header.Get("X-Stub") != "fallback" {
		t.Fatalf("before embedded ready: route=%q err=%v", resp.Header.Get("X-Stub"), err)
	}
	// A transient ready-time suffix failure must not prevent the foreign
	// membership+dial client from switching to embedded tsnet.
	rt.SetTailnet("", &stubRT{name: "tsnet"}, embedded)
	resp, err = rt.RoundTrip(req)
	if err != nil || resp.Header.Get("X-Stub") != "embedded" {
		t.Fatalf("after embedded ready: route=%q err=%v", resp.Header.Get("X-Stub"), err)
	}
	if system.calls != 1 || embedded.calls != 1 {
		t.Fatalf("status calls system=%d embedded=%d", system.calls, embedded.calls)
	}
}

func TestForeignPeerPreReadyFallsBackButAuthoritativeFailureDoesNot(t *testing.T) {
	fallback := &stubRT{name: "fallback"}
	req := reqFor(t, "https://ft.foreign.ts.net/v1/health")
	preReady := &foreignPeerTransport{fallback: fallback, exact: &stubRT{name: "exact"}}
	resp, err := preReady.RoundTrip(req)
	if err != nil || resp.Header.Get("X-Stub") != "fallback" {
		t.Fatalf("pre-ready route=%v err=%v", resp, err)
	}

	hung := &fakeStatusClient{statusFunc: func(ctx context.Context) (*ipnstate.Status, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	authoritative := &foreignPeerTransport{fallback: fallback, exact: &stubRT{name: "exact"}, client: hung}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = authoritative.RoundTrip(req.WithContext(ctx))
	if err == nil || !strings.Contains(err.Error(), "status unavailable") {
		t.Fatalf("authoritative status err=%v", err)
	}
}

func TestDialAnyCancellationClosesLateConnections(t *testing.T) {
	const racers = 2
	started := make(chan struct{}, racers)
	peers := make(chan net.Conn, racers)
	dialer := dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		a, b := net.Pipe()
		peers <- b
		return a, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := dialAny(ctx, dialer, "tcp", "443", []netip.Addr{netip.MustParseAddr("100.64.0.8"), netip.MustParseAddr("100.64.0.9")})
		done <- err
	}()
	<-started
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("dialAny err=%v", err)
	}
	for range racers {
		peer := <-peers
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("late connection was not closed: %v", err)
		}
		_ = peer.Close()
	}
}

func TestStatusWithTimeoutUnblocksReadyTimeDiscovery(t *testing.T) {
	client := &fakeStatusClient{statusFunc: func(ctx context.Context) (*ipnstate.Status, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	started := time.Now()
	_, err := statusWithTimeout(context.Background(), client, 25*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("status timeout took %v", elapsed)
	}
}

func TestNewRoutedTransportDoesNotDependOnSystemTailscaled(t *testing.T) {
	rt := NewRoutedTransport()
	foreign := rt.foreign.(*foreignPeerTransport)
	if client, _ := foreign.currentClient(); client != nil {
		t.Fatalf("initial foreign client = %T, want nil until embedded tsnet is ready", client)
	}
}

func TestStatusMagicDNSSuffix(t *testing.T) {
	status := &ipnstate.Status{
		MagicDNSSuffix: "legacy.ts.net.",
		CurrentTailnet: &ipnstate.TailnetStatus{MagicDNSSuffix: "Current.TS.NET."},
	}
	if got := StatusMagicDNSSuffix(status); got != "Current.TS.NET" {
		t.Fatalf("suffix=%q", got)
	}
	if got := StatusMagicDNSSuffix(nil); got != "" {
		t.Fatalf("nil suffix=%q", got)
	}
}

func TestForeignPeerStatusCoalescesConcurrentReconnects(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	client := &fakeStatusClient{statusFunc: func(context.Context) (*ipnstate.Status, error) {
		started <- struct{}{}
		<-release
		return peerStatus(
			&ipnstate.PeerStatus{DNSName: "one.foreign.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.8")}},
			&ipnstate.PeerStatus{DNSName: "two.foreign.ts.net.", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}},
		), nil
	}}
	exact := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})
	foreign := &foreignPeerTransport{fallback: &stubRT{name: "fallback"}, exact: exact, client: client}
	errs := make(chan error, 2)
	for _, host := range []string{"one.foreign.ts.net", "two.foreign.ts.net"} {
		host := host
		go func() {
			_, err := foreign.RoundTrip(reqFor(t, "https://"+host+"/v1/events"))
			errs <- err
		}()
	}
	<-started
	client.mu.Lock()
	calls := client.calls
	client.mu.Unlock()
	if calls != 1 {
		t.Fatalf("concurrent status calls=%d, want 1", calls)
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	client.mu.Lock()
	calls = client.calls
	client.mu.Unlock()
	if calls != 1 {
		t.Fatalf("total status calls=%d, want 1", calls)
	}
}

func TestForeignPeerStatusServesBoundedStaleSnapshot(t *testing.T) {
	now := time.Unix(100, 0)
	client := &fakeStatusClient{status: peerStatus(&ipnstate.PeerStatus{DNSName: "ft.foreign.ts.net.", Online: true})}
	foreign := &foreignPeerTransport{client: client, now: func() time.Time { return now }}
	_, generation := foreign.currentClient()
	first, err := foreign.peerStatus(context.Background(), client, generation)
	if err != nil || first == nil {
		t.Fatalf("initial status=%v err=%v", first, err)
	}
	client.mu.Lock()
	client.err = errors.New("backend busy")
	client.status = nil
	client.mu.Unlock()
	now = now.Add(peerStatusFreshFor + time.Millisecond)
	stale, err := foreign.peerStatus(context.Background(), client, generation)
	if err != nil || stale != first {
		t.Fatalf("bounded stale status=%v err=%v", stale, err)
	}
	now = time.Unix(100, 0).Add(peerStatusStaleFor + time.Millisecond)
	if _, err := foreign.peerStatus(context.Background(), client, generation); err == nil || !strings.Contains(err.Error(), "backend busy") {
		t.Fatalf("expired stale err=%v", err)
	}
}

func TestForeignPeerClientSwapInvalidatesStatusCache(t *testing.T) {
	now := time.Unix(100, 0)
	clientA := &fakeStatusClient{status: peerStatus()}
	clientB := &fakeStatusClient{status: peerStatus()}
	foreign := &foreignPeerTransport{client: clientA, now: func() time.Time { return now }}
	_, generationA := foreign.currentClient()
	if _, err := foreign.peerStatus(context.Background(), clientA, generationA); err != nil {
		t.Fatal(err)
	}
	foreign.setClient(clientB)
	_, generationB := foreign.currentClient()
	if _, err := foreign.peerStatus(context.Background(), clientB, generationB); err != nil {
		t.Fatal(err)
	}
	if clientA.calls != 1 || clientB.calls != 1 {
		t.Fatalf("status calls A=%d B=%d", clientA.calls, clientB.calls)
	}
}

func TestForeignPeerStatusLeaderCancellationDoesNotPoisonWaiter(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := &fakeStatusClient{statusFunc: func(ctx context.Context) (*ipnstate.Status, error) {
		close(started)
		select {
		case <-release:
			return peerStatus(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	foreign := &foreignPeerTransport{client: client}
	_, generation := foreign.currentClient()
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() { _, err := foreign.peerStatus(leaderCtx, client, generation); leaderDone <- err }()
	<-started
	waiterDone := make(chan error, 1)
	go func() { _, err := foreign.peerStatus(context.Background(), client, generation); waiterDone <- err }()
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err=%v", err)
	}
	close(release)
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter inherited leader cancellation: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("status calls=%d, want 1", client.calls)
	}
}
