package httpz

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEEventBoundariesSurviveGzip is the load-bearing test for R1: an SSE
// event flushed by the handler must be decodable by the client before the
// next event exists. If gzip buffered across events, the read below would
// block and the test would time out — which is exactly the failure mode
// that would break #522's stall detection and #519's coalescing behavior.
func TestSSEEventBoundariesSurviveGzip(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprint(w, "event: first\ndata: {\"a\":1}\n\n")
		if err := rc.Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		<-release // no more bytes until the client proves it decoded event 1
		fmt.Fprint(w, "event: second\ndata: {\"b\":2}\n\n")
		_ = rc.Flush()
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	// Explicit Accept-Encoding disables the transport's transparent decode,
	// so this exercises the raw wire.
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length must be absent on a compressed stream, got %q", got)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q", resp.Header.Get("Vary"))
	}

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(zr)
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				return
			}
			if strings.HasSuffix(sb.String(), "\n\n") {
				done <- sb.String()
				return
			}
		}
	}()
	select {
	case frame := <-done:
		if !strings.Contains(frame, "event: first") {
			t.Fatalf("first frame = %q", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE event was not decodable before the second was written: gzip buffered across the event boundary")
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "event: second") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestNoGzipWithoutAcceptEncoding(t *testing.T) {
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, strings.Repeat("x", 4096))
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed a client that did not ask")
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 4096 {
		t.Fatalf("len=%d", len(body))
	}
}

func TestUpgradeRequestNotWrapped(t *testing.T) {
	var hijackable bool
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hijackable = w.(http.Hijacker)
		if _, ok := w.(*gzipWriter); ok {
			t.Error("upgrade request was wrapped")
		}
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !hijackable {
		t.Fatal("handler lost Hijacker")
	}
}

func TestPassthroughNonCompressibleAndSmall(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		length      string
		wantGzip    bool
	}{
		{"json", "application/json", "", true},
		{"eventstream", "text/event-stream", "", true},
		{"octet (scrollback)", "application/octet-stream", "", true},
		{"png", "image/png", "", false},
		{"short json", "application/json", "10", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
					fmt.Fprint(w, strings.Repeat("y", 10))
					return
				}
				fmt.Fprint(w, strings.Repeat("y", 4096))
			})))
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			srv.Close()
			if got := resp.Header.Get("Content-Encoding") == "gzip"; got != tc.wantGzip {
				t.Fatalf("gzip=%v want %v (ce=%q)", got, tc.wantGzip, resp.Header.Get("Content-Encoding"))
			}
		})
	}
}

func TestAlreadyEncodedPassthrough(t *testing.T) {
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		fmt.Fprint(zw, strings.Repeat("z", 4096))
		zw.Close()
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	zr, err := gzip.NewReader(resp.Body) // must decode in ONE layer
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 4096 {
		t.Fatalf("double-encoded: len=%d", len(body))
	}
}

// TestGoClientTransparentDecode covers the peer link and the CLI: neither
// sets Accept-Encoding, so net/http negotiates gzip and decodes it
// transparently. This is what makes the change invisible to `as=peer`.
func TestGoClientTransparentDecode(t *testing.T) {
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"pad":"`+strings.Repeat("p", 4096)+`"}`)
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), `{"ok":true`) {
		t.Fatalf("body=%.40q", body)
	}
	if resp.Uncompressed != true {
		t.Fatal("expected transparent decompression")
	}
}

// TestResponseControllerReachesConn guards the SSE write-deadline contract:
// sendSSEFrame/sendSSETransaction set a 10 s deadline through an
// http.ResponseController built on the (now wrapped) writer. Unwrap must
// expose the real writer or every SSE write would lose its stall bound.
func TestResponseControllerReachesConn(t *testing.T) {
	var deadlineErr, flushErr error
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		deadlineErr = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(w, "event: x\ndata: 1\n\n")
		flushErr = rc.Flush()
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if deadlineErr != nil {
		t.Fatalf("SetWriteDeadline through wrapper: %v", deadlineErr)
	}
	if flushErr != nil {
		t.Fatalf("Flush through wrapper: %v", flushErr)
	}
}

func TestRangeRequestNotCompressed(t *testing.T) {
	payload := strings.Repeat("r", 8192)
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.txt", time.Time{}, strings.NewReader(payload))
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-99")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed a ranged response")
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 100 {
		t.Fatalf("len=%d", len(body))
	}
}
