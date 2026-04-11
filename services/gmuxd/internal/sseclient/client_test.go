package sseclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sseServer is a test helper that serves SSE frames.
type sseServer struct {
	*httptest.Server
	mu         sync.Mutex
	frames     []string   // frames queued for the next connection
	expectAuth string     // if set, requires "Authorization: Bearer <expectAuth>"
	clients    chan int   // signals "client connected" by sending 1
	hold       chan struct{} // if set, server holds the stream open until closed
}

func newSSEServer(t *testing.T) *sseServer {
	t.Helper()
	s := &sseServer{
		clients: make(chan int, 8),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		expectAuth := s.expectAuth
		frames := make([]string, len(s.frames))
		copy(frames, s.frames)
		hold := s.hold
		s.mu.Unlock()

		if expectAuth != "" {
			got := r.Header.Get("Authorization")
			if got != "Bearer "+expectAuth {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		select {
		case s.clients <- 1:
		default:
		}
		for _, frame := range frames {
			fmt.Fprint(w, frame)
		}
		flusher.Flush()

		if hold != nil {
			select {
			case <-r.Context().Done():
			case <-hold:
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// setFrames queues the given raw SSE frames for the next connection.
func (s *sseServer) setFrames(frames ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = frames
}

func (s *sseServer) setAuth(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expectAuth = token
}

func (s *sseServer) setHold(ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = ch
}

// waitForEvents invokes Subscribe in a goroutine and returns once
// `want` events have been received or the timeout expires.
func waitForEvents(t *testing.T, c *Client, want int, timeout time.Duration) ([]Event, error) {
	t.Helper()
	var (
		events []Event
		mu     sync.Mutex
	)
	done := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	go func() {
		err := c.Subscribe(ctx, nil, func(ev Event) {
			mu.Lock()
			events = append(events, Event{Type: ev.Type, Data: append([]byte(nil), ev.Data...)})
			n := len(events)
			mu.Unlock()
			if n >= want {
				cancel()
			}
		})
		done <- err
	}()

	select {
	case err := <-done:
		mu.Lock()
		defer mu.Unlock()
		return events, err
	case <-time.After(timeout + 50*time.Millisecond):
		t.Fatal("test timeout")
		return nil, nil
	}
}

func TestSubscribe_SingleEvent(t *testing.T) {
	s := newSSEServer(t)
	s.setFrames("event: hello\ndata: world\n\n")

	c := New(s.URL)
	events, err := waitForEvents(t, c, 1, 500*time.Millisecond)
	if err != nil && !errors.Is(err, ErrStreamEnded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != "hello" || string(events[0].Data) != "world" {
		t.Errorf("event = %+v, want {hello world}", events[0])
	}
}

func TestSubscribe_MultipleEvents(t *testing.T) {
	s := newSSEServer(t)
	s.setFrames(
		"event: a\ndata: 1\n\n",
		"event: b\ndata: 2\n\n",
		"event: c\ndata: 3\n\n",
	)

	c := New(s.URL)
	events, _ := waitForEvents(t, c, 3, 500*time.Millisecond)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, want := range []struct {
		typ, data string
	}{
		{"a", "1"}, {"b", "2"}, {"c", "3"},
	} {
		if events[i].Type != want.typ || string(events[i].Data) != want.data {
			t.Errorf("event[%d] = %+v, want {%s %s}", i, events[i], want.typ, want.data)
		}
	}
}

func TestSubscribe_CommentLinesSkipped(t *testing.T) {
	s := newSSEServer(t)
	// Comment lines (starting with ':') are SSE keepalives. They must
	// not produce events but also must not break parsing of surrounding
	// real events.
	s.setFrames(
		": keepalive\n\n",
		"event: real\ndata: payload\n\n",
		": another\n\n",
	)

	c := New(s.URL)
	events, _ := waitForEvents(t, c, 1, 500*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != "real" || string(events[0].Data) != "payload" {
		t.Errorf("event = %+v, want {real payload}", events[0])
	}
}

func TestSubscribe_Unauthorized(t *testing.T) {
	s := newSSEServer(t)
	s.setAuth("correct-token")

	c := New(s.URL, WithBearerToken("wrong-token"))
	err := c.Subscribe(context.Background(), nil, func(Event) {})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestSubscribe_BearerTokenInjected(t *testing.T) {
	s := newSSEServer(t)
	s.setAuth("right-token")
	s.setFrames("event: ok\ndata: 1\n\n")

	c := New(s.URL, WithBearerToken("right-token"))
	events, _ := waitForEvents(t, c, 1, 500*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("auth failed: got %d events, want 1", len(events))
	}
}

func TestSubscribe_EmptyTokenSkipsAuthHeader(t *testing.T) {
	// A zero bearer token must not send an "Authorization: Bearer "
	// header. Tailscale-discovered peers pass "" here and rely on
	// WhoIs identity instead.
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ping\ndata: ok\n\n")
	}))
	defer ts.Close()

	c := New(ts.URL, WithBearerToken(""))
	_, _ = waitForEvents(t, c, 1, 500*time.Millisecond)
	if gotHeader != "" {
		t.Errorf("Authorization header = %q, want empty", gotHeader)
	}
}

func TestSubscribe_ConnectedCallback(t *testing.T) {
	s := newSSEServer(t)
	s.setFrames("event: x\ndata: y\n\n")

	c := New(s.URL)

	var connectCount int32
	var eventsBeforeConnect int32
	var eventsAfterConnect int32

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = c.Subscribe(ctx,
			func() {
				atomic.AddInt32(&connectCount, 1)
			},
			func(Event) {
				if atomic.LoadInt32(&connectCount) == 0 {
					atomic.AddInt32(&eventsBeforeConnect, 1)
				} else {
					atomic.AddInt32(&eventsAfterConnect, 1)
				}
				cancel()
			},
		)
		close(done)
	}()

	<-done

	if got := atomic.LoadInt32(&connectCount); got != 1 {
		t.Errorf("connect callback invoked %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&eventsBeforeConnect); got != 0 {
		t.Errorf("events before connect callback: %d, want 0", got)
	}
	if got := atomic.LoadInt32(&eventsAfterConnect); got != 1 {
		t.Errorf("events after connect callback: %d, want 1", got)
	}
}

func TestSubscribe_ContextCancel(t *testing.T) {
	s := newSSEServer(t)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s.setHold(hold)

	c := New(s.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.Subscribe(ctx, nil, func(Event) {})
	}()

	// Wait for client to connect, then cancel.
	select {
	case <-s.clients:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("client did not connect")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Subscribe did not return after cancel")
	}
}

func TestSubscribe_StreamEnded(t *testing.T) {
	// Server closes cleanly with no events; Subscribe should return
	// ErrStreamEnded so the caller knows to reconnect.
	s := newSSEServer(t)
	// No frames, no hold: handler returns immediately, body EOFs.

	c := New(s.URL)
	err := c.Subscribe(context.Background(), nil, func(Event) {})
	if !errors.Is(err, ErrStreamEnded) {
		t.Errorf("err = %v, want ErrStreamEnded", err)
	}
}

func TestSubscribe_OversizedEvent(t *testing.T) {
	// A single data: line larger than the buffer must produce a
	// protocol error, not silent truncation. Use a small buffer so
	// the test payload stays manageable.
	huge := strings.Repeat("x", 2048)
	s := newSSEServer(t)
	s.setFrames("event: big\ndata: " + huge + "\n\n")

	c := New(s.URL, WithBufferSize(512))
	err := c.Subscribe(context.Background(), nil, func(Event) {})
	if err == nil || errors.Is(err, ErrStreamEnded) {
		t.Errorf("err = %v, want a protocol error", err)
	}
}

func TestSubscribe_EventWithoutData(t *testing.T) {
	// An "event:" line with no "data:" line is per-spec a no-op event
	// (dispatched as MessageEvent with empty data on the browser).
	// Our decoder requires a data: line to fire the handler, matching
	// the behavior of the peering decoder it replaces.
	s := newSSEServer(t)
	s.setFrames(
		"event: ghost\n\n",
		"event: real\ndata: payload\n\n",
	)

	c := New(s.URL)
	events, _ := waitForEvents(t, c, 1, 500*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != "real" {
		t.Errorf("event = %+v, want {real ...}", events[0])
	}
}

func TestSubscribe_DataWithoutEvent(t *testing.T) {
	// A "data:" line with no preceding "event:" line has no dispatch
	// target and must be dropped. Again matches peering's behavior.
	s := newSSEServer(t)
	s.setFrames(
		"data: orphan\n\n",
		"event: real\ndata: payload\n\n",
	)

	c := New(s.URL)
	events, _ := waitForEvents(t, c, 1, 500*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != "real" || string(events[0].Data) != "payload" {
		t.Errorf("event = %+v, want {real payload}", events[0])
	}
}

func TestSubscribe_CustomHeader(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ok\ndata: 1\n\n")
	}))
	defer ts.Close()

	c := New(ts.URL, WithHeader("X-Custom", "hello"))
	_, _ = waitForEvents(t, c, 1, 500*time.Millisecond)
	if got != "hello" {
		t.Errorf("X-Custom = %q, want hello", got)
	}
}

func TestSubscribe_AcceptHeader(t *testing.T) {
	// Accept: text/event-stream is set automatically by New.
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer ts.Close()

	c := New(ts.URL)
	_ = c.Subscribe(context.Background(), nil, func(Event) {})
	if got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
}

func TestSubscribe_NilHandlerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	c := New("http://localhost")
	_ = c.Subscribe(context.Background(), nil, nil)
}

func TestSubscribe_RequestURLInvalid(t *testing.T) {
	c := New("://not-a-url")
	err := c.Subscribe(context.Background(), nil, func(Event) {})
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
	if errors.Is(err, ErrStreamEnded) || errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want a generic request error", err)
	}
}

// ── Idle timeout ──────────────────────────────────────────────────

func TestSubscribe_IdleTimeout_SilentServer(t *testing.T) {
	// Server accepts the connection, sends headers, then goes silent.
	// Subscribe should return ErrStreamIdle after the idle timeout.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		// Block until client disconnects.
		<-r.Context().Done()
	}))
	defer ts.Close()

	c := New(ts.URL, WithIdleTimeout(100*time.Millisecond))
	var connected bool
	err := c.Subscribe(context.Background(),
		func() { connected = true },
		func(Event) { t.Error("unexpected event") },
	)
	if !connected {
		t.Error("connected callback should have fired")
	}
	if !errors.Is(err, ErrStreamIdle) {
		t.Errorf("err = %v, want ErrStreamIdle", err)
	}
}

func TestSubscribe_IdleTimeout_ActiveServerResets(t *testing.T) {
	// Server sends events faster than the idle timeout. The timeout
	// should never fire.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "event: ping\ndata: %d\n\n", i)
			f.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer ts.Close()

	c := New(ts.URL, WithIdleTimeout(200*time.Millisecond))
	var count int
	err := c.Subscribe(context.Background(), nil, func(ev Event) {
		count++
	})
	// Server closes stream cleanly after 5 events.
	if !errors.Is(err, ErrStreamEnded) {
		t.Errorf("err = %v, want ErrStreamEnded", err)
	}
	if count != 5 {
		t.Errorf("got %d events, want 5", count)
	}
}

func TestSubscribe_IdleTimeout_CallerCancelTakesPriority(t *testing.T) {
	// If the caller cancels before the idle timeout, the error should
	// be context.Canceled, not ErrStreamIdle.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel quickly, well before the 5s idle timeout.
	time.AfterFunc(50*time.Millisecond, cancel)

	c := New(ts.URL, WithIdleTimeout(5*time.Second))
	err := c.Subscribe(ctx, nil, func(Event) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestSubscribe_IdleTimeout_CommentResetsDeadline(t *testing.T) {
	// Server sends comment lines (keepalives). Even though they don't
	// produce events, they should reset the idle timer because they
	// prove the connection is alive.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		// Send comments every 50ms for 300ms, then one real event, then close.
		for i := 0; i < 6; i++ {
			fmt.Fprintln(w, ": keepalive")
			f.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintln(w, "event: done")
		fmt.Fprintln(w, "data: ok")
		fmt.Fprintln(w)
		f.Flush()
	}))
	defer ts.Close()

	// Idle timeout is 150ms; the 50ms comment interval keeps it alive
	// for 300ms total, then the real event arrives.
	c := New(ts.URL, WithIdleTimeout(150*time.Millisecond))
	var got []string
	err := c.Subscribe(context.Background(), nil, func(ev Event) {
		got = append(got, ev.Type)
	})
	if !errors.Is(err, ErrStreamEnded) {
		t.Errorf("err = %v, want ErrStreamEnded", err)
	}
	if len(got) != 1 || got[0] != "done" {
		t.Errorf("events = %v, want [done]", got)
	}
}

func TestSubscribe_NoIdleTimeout_InfiniteByDefault(t *testing.T) {
	// Without WithIdleTimeout, the client should block until the
	// server closes or the caller cancels. Here we cancel after 50ms
	// to prove no spurious ErrStreamIdle.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := New(ts.URL) // no idle timeout
	err := c.Subscribe(ctx, nil, func(Event) {})
	if errors.Is(err, ErrStreamIdle) {
		t.Error("got ErrStreamIdle without idle timeout configured")
	}
}

func TestSubscribe_ServerReturns500(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := New(ts.URL)
	err := c.Subscribe(context.Background(), nil, func(Event) {})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrStreamEnded) {
		t.Errorf("err = %v, want a status error", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want status code in message", err)
	}
}
