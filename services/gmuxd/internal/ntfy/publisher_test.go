package ntfy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublisherSendsPrivacySafeRequest(t *testing.T) {
	request := make(chan *http.Request, 1)
	body := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
		request <- r.Clone(context.Background())
		body <- string(data)
	}))
	defer server.Close()

	publisher := newTestPublisher(t, Config{
		ServerURL: server.URL,
		Topic:     "gmux_secret_topic",
		Token:     "secret-token",
		Priority:  4,
		Tags:      []string{"gmux", "white_check_mark"},
		ClickURL:  "https://gmux.example.net/",
		Timeout:   time.Second,
		Hostname:  "desktop",
	})
	defer publisher.Close()

	if !publisher.Notify(Message{Kind: KindFinished, SessionID: "1vshk4fu", Adapter: "pi"}) {
		t.Fatal("Notify() dropped request")
	}

	select {
	case got := <-request:
		if got.Method != http.MethodPost || got.URL.Path != "/gmux_secret_topic" {
			t.Errorf("request = %s %s", got.Method, got.URL.Path)
		}
		if got.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type = %q", got.Header.Get("Content-Type"))
		}
		if got.Header.Get("Title") != "gmux: task finished" || got.Header.Get("Priority") != "4" {
			t.Errorf("notification headers = %#v", got.Header)
		}
		if got.Header.Get("Tags") != "gmux,white_check_mark" || got.Header.Get("Click") != "https://gmux.example.net/" {
			t.Errorf("optional headers = %#v", got.Header)
		}
		if got.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("Authorization = %q", got.Header.Get("Authorization"))
		}
	case <-time.After(time.Second):
		t.Fatal("request not received")
	}
	if got := <-body; got != "Host desktop · pi session 1vshk4fu needs attention." {
		t.Errorf("body = %q", got)
	}
}

func TestPublisherBasicAuthAndUnread(t *testing.T) {
	got := make(chan struct {
		auth  string
		title string
		body  string
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		got <- struct {
			auth  string
			title string
			body  string
		}{r.Header.Get("Authorization"), r.Header.Get("Title"), string(body)}
	}))
	defer server.Close()

	publisher := newTestPublisher(t, Config{ServerURL: server.URL, Topic: "topic", Username: "user", Password: "pass", Priority: 3, Timeout: time.Second, Hostname: "host"})
	defer publisher.Close()
	publisher.Notify(Message{Kind: KindUnread, SessionID: "abc", Adapter: "shell"})

	result := <-got
	if result.auth != "Basic dXNlcjpwYXNz" || result.title != "gmux: new output" || result.body != "Host host · shell session abc needs attention." {
		t.Fatalf("request = %+v", result)
	}
}

func TestPublisherCoalesced(t *testing.T) {
	got := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		got <- r.Header.Get("Title") + "\n" + string(body)
	}))
	defer server.Close()

	publisher := newTestPublisher(t, Config{ServerURL: server.URL, Topic: "topic", Priority: 3, Timeout: time.Second, Hostname: "host"})
	defer publisher.Close()
	publisher.Notify(Message{Kind: KindCoalesced, Count: 4})
	if value := <-got; value != "gmux\nHost host · 4 sessions need attention." {
		t.Fatalf("notification = %q", value)
	}
}

func TestPublisherDoesNotFollowRedirectOrRetry(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		if r.URL.Path == "/topic" {
			http.Redirect(w, r, "/credential-target", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	logs := make(chan string, 1)
	publisher := newTestPublisher(t, Config{ServerURL: server.URL, Topic: "topic", Token: "secret", Priority: 3, Timeout: time.Second}, WithLogger(func(format string, args ...any) {
		logs <- fmt.Sprintf(format, args...)
	}))
	defer publisher.Close()

	publisher.Notify(Message{Kind: KindUnread, SessionID: "abc"})
	if logLine := <-logs; logLine != "ntfy: publish failed class=http_307" {
		t.Fatalf("log = %q", logLine)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestPublisherTimeoutMakesOneAttemptAndRedactsError(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		<-release
	}))
	defer server.Close()
	defer close(release)
	logs := make(chan string, 1)
	publisher := newTestPublisher(t, Config{ServerURL: server.URL, Topic: "secret-topic", Token: "distinctive-token", Priority: 3, Timeout: 30 * time.Millisecond}, WithLogger(func(format string, args ...any) {
		logs <- fmt.Sprintf(format, args...)
	}))
	defer publisher.Close()

	publisher.Notify(Message{Kind: KindUnread, SessionID: "abc"})
	logLine := <-logs
	if !strings.Contains(logLine, "class=timeout") || strings.Contains(logLine, "distinctive-token") || strings.Contains(logLine, "secret-topic") {
		t.Fatalf("unsafe/unexpected log = %q", logLine)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly one", requests)
	}
}

func TestPublisherNotifyIsNonBlockingAndCloseCancelsRequest(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		close(canceled)
		return nil, req.Context().Err()
	})
	publisher := newTestPublisher(t, Config{ServerURL: "https://example.test", Topic: "topic", Priority: 3, Timeout: time.Minute}, WithTransport(transport), WithLogger(func(string, ...any) {}))

	returned := make(chan bool, 1)
	go func() {
		returned <- publisher.Notify(Message{Kind: KindUnread, SessionID: "first"})
	}()
	select {
	case accepted := <-returned:
		if !accepted {
			t.Fatal("Notify() unexpectedly dropped request")
		}
	case <-time.After(time.Second):
		t.Fatal("Notify() blocked on HTTP")
	}
	<-started

	closed := make(chan struct{})
	go func() {
		publisher.Close()
		close(closed)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Close() did not cancel the request")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close() did not join the request goroutine")
	}
}

func TestPublisherDropsWhenBusyWithoutQueueing(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer server.Close()
	logs := make(chan string, 1)
	publisher := newTestPublisher(t, Config{ServerURL: server.URL, Topic: "topic", Priority: 3, Timeout: time.Minute}, WithConcurrency(1), WithLogger(func(format string, args ...any) {
		logs <- fmt.Sprintf(format, args...)
	}))

	if !publisher.Notify(Message{Kind: KindUnread, SessionID: "first"}) {
		t.Fatal("first request dropped")
	}
	<-started
	if publisher.Notify(Message{Kind: KindUnread, SessionID: "second"}) {
		t.Fatal("second request should be dropped, not queued")
	}
	if logLine := <-logs; logLine != "ntfy: publish dropped class=busy" {
		t.Fatalf("log = %q", logLine)
	}
	close(release)
	publisher.Close()
	if publisher.Notify(Message{Kind: KindUnread, SessionID: "third"}) {
		t.Fatal("closed publisher accepted request")
	}
}

func TestFormatMessageRemovesControlsAndTruncatesUTF8(t *testing.T) {
	_, body := formatMessage("host\r\nInjected", Message{Kind: KindUnread, SessionID: strings.Repeat("界", 300), Adapter: "pi\nBad"})
	if strings.ContainsAny(body, "\r\n") {
		t.Fatalf("body contains control characters: %q", body)
	}
	if len(body) > 512 || !strings.Contains(body, "Host host  Injected") {
		t.Fatalf("body is not safely bounded/cleaned: len=%d body=%q", len(body), body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestPublisher(t *testing.T, cfg Config, opts ...Option) *Publisher {
	t.Helper()
	publisher, err := New(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}
