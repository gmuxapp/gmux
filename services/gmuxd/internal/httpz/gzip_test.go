package httpz

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rawGet performs a request that bypasses the transport's transparent
// gzip handling, so the test observes the actual wire representation.
func rawGet(t *testing.T, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestSSEEventBoundariesSurviveGzip is the load-bearing test: an SSE event
// flushed by the handler must be decodable by the client before the next
// event exists. If gzip buffered across events, the read below would block
// and the test would time out — which is exactly the failure mode that
// would break the frontend's stall detection (#522) and make coalescing
// (#519) invisible.
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

	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
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

// TestFlushOrdering pins the wire-level reason the previous test passes:
// every Flush ends a deflate block with a Z_SYNC_FLUSH marker (an empty
// stored block, 00 00 FF FF), so the bytes on the wire after each flush
// are a self-contained decodable prefix. Ten flushed events → ten markers.
func TestFlushOrdering(t *testing.T) {
	const events = 10
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "event: e%d\ndata: %s\n\n", i, strings.Repeat("x", 300))
			if err := rc.Flush(); err != nil {
				t.Errorf("flush %d: %v", i, err)
			}
		}
	})))
	defer srv.Close()
	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
	wire, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(wire, []byte{0, 0, 0xff, 0xff}); n < events {
		t.Fatalf("found %d sync-flush markers on the wire, want >= %d", n, events)
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                        false,
		"identity":                false,
		"gzip":                    true,
		"GZIP":                    true,
		"gzip, deflate, br, zstd": true,
		"br, gzip;q=0.5":          true,
		"gzip;q=0, identity":      false,
		"gzip; q=0.0":             false,
		"br":                      false,
		"*":                       false, // conservative: only an explicit gzip token opts in
	}
	for accept, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if accept != "" {
			r.Header.Set("Accept-Encoding", accept)
		}
		if got := AcceptsGzip(r); got != want {
			t.Errorf("AcceptsGzip(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestNegotiation(t *testing.T) {
	cases := []struct {
		name     string
		accept   string
		wantGzip bool
	}{
		{"identity", "identity", false},
		{"gzip", "gzip", true},
		{"browser list", "gzip, deflate, br, zstd", true},
		{"case", "GZIP", true},
		{"q=0 refusal", "gzip;q=0, identity", false},
		{"q=0.5", "gzip;q=0.5", true},
		{"other only", "br", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, strings.Repeat("x", 4096))
			})))
			defer srv.Close()
			resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": tc.accept})
			body, _ := io.ReadAll(resp.Body)
			if got := resp.Header.Get("Content-Encoding") == "gzip"; got != tc.wantGzip {
				t.Fatalf("gzip=%v want %v", got, tc.wantGzip)
			}
			if !tc.wantGzip && len(body) != 4096 {
				t.Fatalf("identity body len=%d", len(body))
			}
			// Vary is set either way: the representation depends on the header.
			if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
				t.Fatalf("Vary = %q", resp.Header.Get("Vary"))
			}
		})
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
	resp := rawGet(t, srv.URL, map[string]string{
		"Accept-Encoding": "gzip",
		"Connection":      "Upgrade",
		"Upgrade":         "websocket",
	})
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("upgrade response carries Content-Encoding")
	}
	if !hijackable {
		t.Fatal("handler lost Hijacker")
	}
}

func TestHeadNotWrapped(t *testing.T) {
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*gzipWriter); ok {
			t.Error("HEAD request was wrapped")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodHead, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "4096" {
		t.Fatalf("HEAD headers changed: %v", resp.Header)
	}
}

func TestContentTypeAllowlist(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		length      string
		wantGzip    bool
	}{
		{"json", "application/json", "", true},
		{"json with charset", "application/json; charset=utf-8", "", true},
		{"eventstream", "text/event-stream", "", true},
		{"html", "text/html; charset=utf-8", "", true},
		{"octet (scrollback)", "application/octet-stream", "", true},
		{"svg", "image/svg+xml", "", true},
		{"png", "image/png", "", false},
		{"zip", "application/zip", "", false},
		{"short json", "application/json", "10", false},
		{"declared long json", "application/json", "4096", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}
				n := 4096
				if tc.length == "10" {
					n = 10
				}
				fmt.Fprint(w, strings.Repeat("y", n))
			})))
			defer srv.Close()
			resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
			n, _ := io.Copy(io.Discard, resp.Body)
			if got := resp.Header.Get("Content-Encoding") == "gzip"; got != tc.wantGzip {
				t.Fatalf("gzip=%v want %v (ce=%q)", got, tc.wantGzip, resp.Header.Get("Content-Encoding"))
			}
			// The handler's identity Content-Length must never leak onto a
			// compressed body. net/http may compute one for a small,
			// fully-buffered body — then it is the compressed length.
			if cl := resp.Header.Get("Content-Length"); tc.wantGzip && cl != "" && cl != fmt.Sprint(n) {
				t.Fatalf("Content-Length %q on a %d-byte compressed body", cl, n)
			}
		})
	}
}

// TestSmallUndeclaredBodyStaysIdentity: a 72-byte JSON error envelope with
// no Content-Length must not grow to 89 bytes on the wire. The wrapper
// buffers an undeclared body until it either proves itself (>= 512 B, then
// the buffered prefix is compressed too) or ends.
func TestSmallUndeclaredBodyStaysIdentity(t *testing.T) {
	small := `{"ok":false,"error":{"code":"not_found","message":"session not found"}}`
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/small":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, small)
		case "/empty":
			w.WriteHeader(http.StatusOK)
		case "/grows":
			// Several sub-threshold writes that add up past it.
			for i := 0; i < 20; i++ {
				fmt.Fprintf(w, `{"row":%d,"pad":"%s"}`, i, strings.Repeat("p", 40))
			}
		}
	})))
	defer srv.Close()

	resp := rawGet(t, srv.URL+"/small", map[string]string{"Accept-Encoding": "gzip"})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Encoding") != "" || string(body) != small {
		t.Fatalf("small: status=%d ce=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
	}
	if resp.Header.Get("Content-Length") != fmt.Sprint(len(small)) {
		t.Fatalf("small: Content-Length = %q", resp.Header.Get("Content-Length"))
	}

	empty := rawGet(t, srv.URL+"/empty", map[string]string{"Accept-Encoding": "gzip"})
	io.Copy(io.Discard, empty.Body)
	if empty.StatusCode != http.StatusOK || empty.Header.Get("Content-Encoding") != "" || empty.Header.Get("Content-Length") != "0" {
		t.Fatalf("empty: %d %v", empty.StatusCode, empty.Header)
	}

	grows := rawGet(t, srv.URL+"/grows", map[string]string{"Accept-Encoding": "gzip"})
	if grows.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("body that crossed the threshold was not compressed")
	}
	zr, err := gzip.NewReader(grows.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(zr)
	if !strings.HasPrefix(string(decoded), `{"row":0,`) || strings.Count(string(decoded), `{"row":`) != 20 {
		t.Fatalf("buffered prefix lost: %.60q… (%d rows)", decoded, strings.Count(string(decoded), `{"row":`))
	}
}

func TestStatusesNeverCompressed(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified, http.StatusPartialContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(status)
				// A body well past the 512 B floor: the status alone must
				// keep the response identity. (204/304 handlers should not
				// write, but the guard must not depend on that.)
				_, _ = w.Write([]byte(strings.Repeat("s", 4096)))
			})))
			defer srv.Close()
			resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != status || resp.Header.Get("Content-Encoding") != "" {
				t.Fatalf("status=%d ce=%q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
			}
		})
	}
}

func TestAlreadyEncodedPassthrough(t *testing.T) {
	// A bundle-shaped body: enough entropy that its gzip form is far above
	// the 512 B floor (a 4 KB run of one byte compresses to ~40 B and would
	// stay identity for the wrong reason).
	var src strings.Builder
	for i := 0; src.Len() < 256<<10; i++ {
		fmt.Fprintf(&src, "function f%d(a,b){return a*%d+b^%d}\n", i, i*7919, i*104729)
	}
	var pre bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&pre, gzip.BestCompression)
	_, _ = zw.Write([]byte(src.String()))
	_ = zw.Close()
	if pre.Len() < 8*minCompressLength {
		t.Fatalf("fixture too small to be meaningful: %d B", pre.Len())
	}
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(pre.Len()))
		_, _ = w.Write(pre.Bytes())
	})))
	defer srv.Close()
	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
	wire, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire, pre.Bytes()) {
		t.Fatalf("precompressed body altered on the wire (%d vs %d bytes): double-encoded", len(wire), pre.Len())
	}
	zr, err := gzip.NewReader(bytes.NewReader(wire)) // must decode in ONE layer
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != src.String() {
		t.Fatalf("decoded body differs (len=%d)", len(body))
	}
}

// TestGoClientTransparentDecode covers the peer link and the CLI over TCP:
// neither sets Accept-Encoding, so net/http negotiates gzip and decodes it
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
	if !resp.Uncompressed {
		t.Fatal("expected transparent decompression")
	}
}

// TestLargeBodyRoundTrip is the scrollback shape: a multi-megabyte body of
// mixed text and escape sequences, written in the broker's io.Copy chunks,
// must decode to exactly the identity bytes.
func TestLargeBodyRoundTrip(t *testing.T) {
	var payload bytes.Buffer
	for i := 0; payload.Len() < 3<<20; i++ {
		fmt.Fprintf(&payload, "\x1b[%dm line %d: the quick brown fox jumps over the lazy dog \x1b[0m\r\n", 30+i%8, i)
	}
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, bytes.NewReader(payload.Bytes()))
	})))
	defer srv.Close()
	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("not compressed")
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload.Bytes()) {
		t.Fatalf("decoded body differs: got %d bytes want %d", len(got), payload.Len())
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
	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
	io.Copy(io.Discard, resp.Body)
	if deadlineErr != nil {
		t.Fatalf("SetWriteDeadline through wrapper: %v", deadlineErr)
	}
	if flushErr != nil {
		t.Fatalf("Flush through wrapper: %v", flushErr)
	}
}

// TestWriteDeadlineErrorSurfacesThroughFlush is the other half of the
// stall contract: when the client stops reading and the deadline trips,
// rc.Flush() must return the error to the handler (the SSE loop treats
// that as "drop this subscriber"). http.ResponseController.Flush prefers
// FlushError; a wrapper exposing only Flush() would swallow the error and
// the handler would learn of the stall from the *next* Write (gzip.Writer
// latches the error), i.e. one frame late on a busy stream, or a heartbeat
// late on an idle one.
//
// The frames are small and compressible (SSE-shaped, ~2 KB) so that the
// bytes reach the socket at Flush, not inside Write: this test asserts the
// error arrives from the flush call specifically, and fails if FlushError
// is removed or returns nil.
func TestWriteDeadlineErrorSurfacesThroughFlush(t *testing.T) {
	frame := fmt.Sprintf("event: snapshot.sessions.batch\ndata: %s\n\n", strings.Repeat(`{"id":"s1","alive":true},`, 80))
	type outcome struct {
		from string
		err  error
	}
	result := make(chan outcome, 1)
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for i := 0; i < 1_000_000; i++ {
			_ = rc.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err := io.WriteString(w, frame); err != nil {
				result <- outcome{"write", err}
				return
			}
			if err := rc.Flush(); err != nil {
				result <- outcome{"flush", err}
				return
			}
		}
		result <- outcome{"none", nil}
	})))
	defer srv.Close()

	// Raw TCP client that sends the request and never reads the response.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n\r\n")

	select {
	case got := <-result:
		if got.err == nil {
			t.Fatal("handler streamed a million frames to a non-reading client; deadline never surfaced")
		}
		if !errors.Is(got.err, os.ErrDeadlineExceeded) {
			t.Fatalf("error = %v, want a deadline error", got.err)
		}
		if got.from != "flush" {
			t.Fatalf("deadline surfaced from %s, want flush: FlushError is not reaching the handler", got.from)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("handler did not observe the write deadline within 20 s")
	}
}

func TestRangeRequestNotCompressed(t *testing.T) {
	payload := strings.Repeat("r", 64<<10)
	t.Run("206 from ServeContent", func(t *testing.T) {
		srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "x.txt", time.Time{}, strings.NewReader(payload))
		})))
		defer srv.Close()
		// Well above the 512 B floor so only the Range/206 guards keep it identity.
		resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=1000-9191"})
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if resp.Header.Get("Content-Encoding") != "" {
			t.Fatal("compressed a ranged response")
		}
		if len(body) != 8192 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-9191/%d", len(payload)) {
			t.Fatalf("len=%d content-range=%q", len(body), resp.Header.Get("Content-Range"))
		}
	})
	t.Run("200 from a handler that ignores Range", func(t *testing.T) {
		// The scrollback broker serves the full body regardless of Range. A
		// client that sent Range is still expecting identity offsets, so the
		// request-level bypass must hold even when no 206 is produced.
		srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, payload)
		})))
		defer srv.Close()
		resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9191"})
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || len(body) != len(payload) {
			t.Fatalf("status=%d ce=%q len=%d", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body))
		}
	})
}

func TestImplicitContentTypeSniff(t *testing.T) {
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Type, no WriteHeader: net/http would sniff text/plain.
		fmt.Fprint(w, strings.Repeat("plain text ", 1024))
	})))
	defer srv.Close()
	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
	io.Copy(io.Discard, resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("sniffed text was not compressed")
	}
}

// TestSSECommitsToGzipBeforeFirstFlush: an event stream must not sit in
// the small-body buffer waiting for 512 bytes — its first event (a 62-byte
// begin marker) has to be on the wire, gzip-encoded, at the first flush.
func TestSSECommitsToGzipBeforeFirstFlush(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		if _, isGzip := w.(*gzipWriter); !isGzip {
			t.Error("not wrapped")
		}
		w.WriteHeader(http.StatusOK)
		if gw := w.(*gzipWriter); gw.zw == nil || gw.pending {
			t.Errorf("event-stream did not commit to gzip at WriteHeader (zw=%v pending=%v)", gw.zw != nil, gw.pending)
		}
		fmt.Fprint(w, "event: snapshot.sessions.begin\ndata: {\"epoch\":1}\n\n")
		_ = rc.Flush()
		<-release
	})))
	defer srv.Close()
	resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("ce=%q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(zr).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if !strings.HasPrefix(line, "event: snapshot.sessions.begin") {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("62-byte first event not delivered: stream was held in the small-body buffer")
	}
	close(release)
}

func TestPooledWriterKeepsLevel(t *testing.T) {
	// Fill the level-1 pool, then check a level-9 response still compresses
	// noticeably better than level 1 would (pools must not cross levels).
	payload := []byte(strings.Repeat("abcabcabd", 20000))
	size := func(level int) int {
		srv := httptest.NewServer(GzipLevel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			for i := 0; i < len(payload); i += 4096 {
				end := min(i+4096, len(payload))
				_, _ = w.Write(payload[i:end])
			}
		}), level))
		defer srv.Close()
		resp := rawGet(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"})
		n, _ := io.Copy(io.Discard, resp.Body)
		return int(n)
	}
	// Fresh writers first, then the pooled ones: with a shared pool the
	// second level-9 response would get the level-1 writer back.
	l1a, l9a := size(1), size(9)
	l1b, l9b := size(1), size(9)
	if l1a != l1b || l9a != l9b {
		t.Fatalf("output not stable across pool reuse: L1 %d/%d, L9 %d/%d", l1a, l1b, l9a, l9b)
	}
	if l9a >= l1a {
		t.Fatalf("level 9 (%d B) not smaller than level 1 (%d B): pool served the wrong level", l9a, l1a)
	}
	// Direct check on the pool itself: a writer put back at one level must
	// never come out at another.
	for level := gzip.BestSpeed; level <= gzip.BestCompression; level++ {
		zw := getWriter(io.Discard, level)
		putWriter(zw, level)
	}
	for level := gzip.BestSpeed; level <= gzip.BestCompression; level++ {
		zw := getWriter(io.Discard, level)
		if got := writerLevel(t, zw); got != level {
			t.Fatalf("pool for level %d returned a level-%d writer", level, got)
		}
		putWriter(zw, level)
	}
}

// writerLevel reads the unexported level a gzip.Writer was created with.
// Adjacent levels can produce identical output on small inputs, so output
// size cannot discriminate them; the field can.
func writerLevel(t *testing.T, zw *gzip.Writer) int {
	t.Helper()
	f := reflect.ValueOf(zw).Elem().FieldByName("level")
	if !f.IsValid() {
		t.Skip("gzip.Writer has no level field in this Go version")
	}
	return int(f.Int())
}
