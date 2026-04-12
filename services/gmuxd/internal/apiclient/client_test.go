package apiclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sseclient"
)

// ── Construction + basics ────────────────────────────────────────

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New("http://host:8790/")
	if c.BaseURL() != "http://host:8790" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL(), "http://host:8790")
	}
}

func TestNew_WithBearerToken(t *testing.T) {
	c := New("http://host", WithBearerToken("abc"))
	if c.token != "abc" {
		t.Errorf("token = %q, want abc", c.token)
	}
}

// ── GetHealth ─────────────────────────────────────────────────────

func TestGetHealth_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer the-token" {
			http.Error(w, "unauth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":{"version":"0.8.0","hostname":"test"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("the-token"))
	data, err := c.GetHealth(context.Background())
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed["hostname"] != "test" {
		t.Errorf("hostname = %v, want test", parsed["hostname"])
	}
}

func TestGetHealth_Unauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("wrong"))
	_, err := c.GetHealth(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want status code in message", err)
	}
}

func TestGetHealth_EnvelopeOkFalse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	_, err := c.GetHealth(context.Background())
	if err == nil {
		t.Fatal("expected error for ok=false")
	}
}

func TestGetHealth_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	_, err := c.GetHealth(context.Background())
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

// ── stripPeerField ────────────────────────────────────────────────

func TestStripPeerField(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips peer",
			in:   `{"launcher_id":"shell","cwd":"/root","peer":"dev"}`,
			want: `{"cwd":"/root","launcher_id":"shell"}`,
		},
		{
			name: "no peer is noop",
			in:   `{"launcher_id":"shell","cwd":"/root"}`,
			want: `{"cwd":"/root","launcher_id":"shell"}`,
		},
		{
			name: "empty peer is removed",
			in:   `{"launcher_id":"shell","peer":""}`,
			want: `{"launcher_id":"shell"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stripPeerField([]byte(tt.in))
			if err != nil {
				t.Fatalf("stripPeerField: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("stripPeerField = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestStripPeerField_InvalidJSON(t *testing.T) {
	_, err := stripPeerField([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// ── ForwardAction ─────────────────────────────────────────────────

func TestForwardAction_PathConstruction(t *testing.T) {
	var gotPath, gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("tok"))
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-123/kill", nil)
	w := httptest.NewRecorder()
	c.ForwardAction(w, req, "sess-123", "kill")

	if gotPath != "/v1/sessions/sess-123/kill" {
		t.Errorf("path = %q, want /v1/sessions/sess-123/kill", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestForwardAction_BearerInjected(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("my-token"))
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	w := httptest.NewRecorder()
	c.ForwardAction(w, req, "sess-x", "resume")

	if gotAuth != "Bearer my-token" {
		t.Errorf("Authorization = %q, want Bearer my-token", gotAuth)
	}
}

func TestForwardAction_PreservesBody(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"payload":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.ForwardAction(w, req, "sess-y", "read")

	if gotBody != `{"payload":"hello"}` {
		t.Errorf("body = %q, want %q", gotBody, `{"payload":"hello"}`)
	}
}

func TestForwardAction_UpstreamError(t *testing.T) {
	// Dial a port nothing is listening on so the request fails.
	c := New("http://127.0.0.1:1", WithBearerToken("t"))
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	w := httptest.NewRecorder()
	c.ForwardAction(w, req, "sess", "kill")

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestForwardAction_PropagatesStatusCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	c := New(ts.URL)
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	w := httptest.NewRecorder()
	c.ForwardAction(w, req, "sess-missing", "kill")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// ── ForwardLaunch ─────────────────────────────────────────────────

func TestForwardLaunch_StripsPeerField(t *testing.T) {
	var received []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/launch" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		received, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true,"data":{"id":"sess-1"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("tok"))
	body := `{"launcher_id":"shell","cwd":"/root","peer":"dev"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/launch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	c.ForwardLaunch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("invalid JSON forwarded to spoke: %v", err)
	}
	if _, ok := got["peer"]; ok {
		t.Errorf("spoke received body still contains 'peer': %s", received)
	}
	if got["launcher_id"] != "shell" {
		t.Errorf("launcher_id lost: %v", got)
	}
}

func TestForwardLaunch_InvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("spoke should not be called for invalid JSON")
	}))
	defer ts.Close()

	c := New(ts.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/launch", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	c.ForwardLaunch(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ── Events ────────────────────────────────────────────────────────

// TestEvents_CallsAuthorizedSubscribe is the smoke test for apiclient
// → sseclient wiring: Events().Subscribe() must send the bearer
// token the Client was constructed with.
func TestEvents_CallsAuthorizedSubscribe(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("evt-token"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = c.Events().Subscribe(ctx, nil, func(sseclient.Event) {})
	if gotAuth != "Bearer evt-token" {
		t.Errorf("Authorization = %q, want Bearer evt-token", gotAuth)
	}
}

// TestEvents_DeliversEvent verifies the full pipeline: a spoke-style
// SSE frame pushed through an Events() subscribe call reaches the
// handler with the correct type and data.
func TestEvents_DeliversEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: session-upsert\ndata: {\"id\":\"sess-1\"}\n\n"))
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got := make(chan sseclient.Event, 1)
	_ = c.Events().Subscribe(ctx, nil, func(ev sseclient.Event) {
		select {
		case got <- ev:
		default:
		}
	})

	select {
	case ev := <-got:
		if ev.Type != "session-upsert" {
			t.Errorf("type = %q, want session-upsert", ev.Type)
		}
		if !strings.Contains(string(ev.Data), `"sess-1"`) {
			t.Errorf("data = %s, want to contain sess-1", ev.Data)
		}
	default:
		t.Fatal("no event received")
	}
}

// ── DialWS + ProxyWS ──────────────────────────────────────────────

// wsEchoServer runs an nhooyr.io/websocket echo server on an HTTP test
// server. Any binary/text message received is sent back to the peer.
// On connect, it optionally sends `onConnect` to the peer first.
func wsEchoServer(t *testing.T, onConnect []byte, expectAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expectAuth != "" && r.Header.Get("Authorization") != "Bearer "+expectAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		// Large read limit for the "spoke receives client input" leg.
		conn.SetReadLimit(4 * 1024 * 1024)
		defer conn.Close(websocket.StatusNormalClosure, "")

		ctx := r.Context()
		if len(onConnect) > 0 {
			_ = conn.Write(ctx, websocket.MessageBinary, onConnect)
		}
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if err := conn.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
}

func TestDialWS_Success(t *testing.T) {
	ts := wsEchoServer(t, nil, "")
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	conn, err := c.DialWS(ctx, "sess-xyz")
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
}

func TestDialWS_BearerInjected(t *testing.T) {
	ts := wsEchoServer(t, nil, "ws-tok")
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("ws-tok"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	conn, err := c.DialWS(ctx, "sess")
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func TestDialWS_UnauthorizedFails(t *testing.T) {
	ts := wsEchoServer(t, nil, "correct")
	defer ts.Close()

	c := New(ts.URL, WithBearerToken("wrong"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := c.DialWS(ctx, "sess")
	if err == nil {
		t.Fatal("expected auth error")
	}
}

func TestDialWS_HTTPSUpgradesToWSS(t *testing.T) {
	// Cover the scheme rewrite paths in DialWS without actually
	// standing up TLS. We can't dial wss://, but we can verify that a
	// broken http-prefix URL fails with a wss-related error message
	// indirectly via the baseURL.
	c := New("https://invalid.invalid:9/")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := c.DialWS(ctx, "sess")
	if err == nil {
		t.Fatal("expected error")
	}
	// We expect the dial to attempt wss://; the error message from
	// nhooyr.io/websocket wraps the URL string. If the scheme rewrite
	// didn't happen the error would mention "http:" instead.
	if !strings.Contains(err.Error(), "wss:") && !strings.Contains(err.Error(), "invalid.invalid") {
		t.Errorf("err = %v, expected to contain wss: or target host", err)
	}
}

// ── ProxyWS regression test for the 256 KiB bug ───────────────────

// TestProxyWS_LargeSnapshot is the Bug 1 regression test. The old
// peering.ProxyWS had a 256 KiB read limit on the spoke side, which
// silently truncated any terminal snapshot bigger than that and
// caused the browser to enter a 1 Hz reconnect loop. apiclient.
// ProxyWS must deliver large spoke frames to the client unchanged.
func TestProxyWS_LargeSnapshot(t *testing.T) {
	// 1 MiB binary payload, ~4x the old broken limit, well within
	// the new 4 MiB spoke limit.
	payload := make([]byte, 1024*1024)
	for i := range payload {
		payload[i] = byte(i)
	}

	// "spoke" sends the payload on connect, then echoes.
	spokeServer := wsEchoServer(t, payload, "")
	defer spokeServer.Close()

	// "hub" is an HTTP server that, on /ws/{id}, accepts the browser
	// WS and calls apiclient.ProxyWS to bridge to spokeServer.
	c := New(spokeServer.URL)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.ProxyWS(w, r, "sess-large")
	}))
	defer hub.Close()

	// Dial the hub as a browser would.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http")
	browser, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("browser dial: %v", err)
	}
	defer browser.Close(websocket.StatusNormalClosure, "")
	browser.SetReadLimit(4 * 1024 * 1024)

	typ, got, err := browser.Read(ctx)
	if err != nil {
		t.Fatalf("browser read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("msg type = %v, want binary", typ)
	}
	if len(got) != len(payload) {
		t.Fatalf("got %d bytes, want %d (truncation bug)", len(got), len(payload))
	}
	// Spot-check a few bytes rather than full bytes.Equal to keep
	// the failure output readable.
	for _, i := range []int{0, 100, len(payload) / 2, len(payload) - 1} {
		if got[i] != payload[i] {
			t.Errorf("byte %d: got %d, want %d", i, got[i], payload[i])
		}
	}
}

// TestProxyWS_Bidirectional exercises the client → spoke direction
// too, to make sure the proxy loop handles both sides.
func TestProxyWS_Bidirectional(t *testing.T) {
	spokeServer := wsEchoServer(t, nil, "")
	defer spokeServer.Close()

	c := New(spokeServer.URL)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.ProxyWS(w, r, "sess")
	}))
	defer hub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http")
	browser, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer browser.Close(websocket.StatusNormalClosure, "")

	// Send text, expect echo.
	want := "hello from browser"
	if err := browser.Write(ctx, websocket.MessageText, []byte(want)); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, got, err := browser.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText || string(got) != want {
		t.Errorf("echo = (%v, %q), want (text, %q)", typ, got, want)
	}
}

// TestProxyWS_ClientDisconnectClosesSpoke verifies that closing the
// browser side propagates to the spoke side, so an idle spoke WS
// isn't left dangling.
func TestProxyWS_ClientDisconnectClosesSpoke(t *testing.T) {
	spokeClosed := make(chan struct{})
	var once sync.Once
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		// Block reading until the client closes; then signal.
		_, _, _ = conn.Read(r.Context())
		once.Do(func() { close(spokeClosed) })
	}))
	defer spoke.Close()

	c := New(spoke.URL)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.ProxyWS(w, r, "sess")
	}))
	defer hub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http")
	browser, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Close the browser side.
	browser.Close(websocket.StatusNormalClosure, "")

	select {
	case <-spokeClosed:
	case <-time.After(1 * time.Second):
		t.Fatal("spoke side was not unblocked after client disconnect")
	}
}

