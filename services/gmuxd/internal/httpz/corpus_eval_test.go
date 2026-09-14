package httpz

// SPIKE R1 measurement harness. Replays a real captured SSE bootstrap
// (GMUX_SSE_CORPUS=/path/to/boot.sse — a read-only capture of
// GET /v1/events?session_stream=3) through the actual middleware with the
// actual per-event flush discipline of sendSSETransaction, and reports
// wire bytes and encode CPU.
//
//	go test ./internal/httpz -run TestCorpus -v
//	go test ./internal/httpz -run xxx -bench Corpus -benchtime 1x

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type sseEvent struct {
	name string
	data []byte
}

func loadCorpus(tb testing.TB) []sseEvent {
	tb.Helper()
	path := os.Getenv("GMUX_SSE_CORPUS")
	if path == "" {
		tb.Skip("set GMUX_SSE_CORPUS to a captured /v1/events body")
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var events []sseEvent
	var cur sseEvent
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, "event: "):
			cur.name = strings.TrimPrefix(trimmed, "event: ")
		case strings.HasPrefix(trimmed, "data: "):
			cur.data = []byte(strings.TrimPrefix(trimmed, "data: "))
		case trimmed == "" && cur.name != "":
			events = append(events, cur)
			cur = sseEvent{}
		}
		if err != nil {
			break
		}
	}
	return events
}

// writeEvents mirrors sendSSETransaction/sendSSEFrame: one write + one
// flush per event when perEventFlush, otherwise a single flush at the end.
func writeEvents(w http.ResponseWriter, events []sseEvent, perEventFlush bool) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
		if perEventFlush {
			_ = rc.Flush()
		}
	}
	if !perEventFlush {
		_ = rc.Flush()
	}
}

func rawSize(events []sseEvent) int {
	n := 0
	for _, e := range events {
		n += len("event: ") + len(e.name) + len("\ndata: ") + len(e.data) + len("\n\n")
	}
	return n
}

// serveAndCount runs one subscriber through the middleware and returns the
// bytes that actually crossed the socket.
func serveAndCount(tb testing.TB, events []sseEvent, level int, perEventFlush bool, gzipOn bool) (wire int, elapsed time.Duration) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeEvents(w, events, perEventFlush) })
	var handler http.Handler = h
	if gzipOn {
		handler = GzipLevel(h, level)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if gzipOn {
		req.Header.Set("Accept-Encoding", "gzip")
	} else {
		req.Header.Set("Accept-Encoding", "identity")
	}
	start := time.Now()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		tb.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	elapsed = time.Since(start)
	if err != nil {
		tb.Fatal(err)
	}
	if gzipOn && resp.Header.Get("Content-Encoding") != "gzip" {
		tb.Fatalf("expected gzip, got %q", resp.Header.Get("Content-Encoding"))
	}
	return int(n), elapsed
}

func TestCorpusBootstrapBytes(t *testing.T) {
	events := loadCorpus(t)
	raw := rawSize(events)
	t.Logf("corpus: %d events, %d raw bytes", len(events), raw)

	// Sanity: the decoded stream must be byte-identical to the raw one.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeEvents(w, events, true) })
	srv := httptest.NewServer(Gzip(h))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, e := range events {
		fmt.Fprintf(&want, "event: %s\ndata: %s\n\n", e.name, e.data)
	}
	if string(got) != want.String() {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), want.Len())
	}
	t.Logf("round trip: %d bytes identical after decode", len(got))

	fmt.Printf("\n=== R1 bootstrap bytes on the real corpus (%d events, %d raw) ===\n", len(events), raw)
	fmt.Printf("%-34s %12s %8s %10s\n", "variant", "wire bytes", "ratio", "client ms")
	base, ms := serveAndCount(t, events, 0, true, false)
	fmt.Printf("%-34s %12d %8.2f %10.1f\n", "identity (today)", base, 1.0, float64(ms.Microseconds())/1000)
	for _, level := range []int{1, 5, 6, 7, 8, 9} {
		for _, perEvent := range []bool{true, false} {
			n, ms := serveAndCount(t, events, level, perEvent, true)
			label := fmt.Sprintf("gzip-%d flush/event", level)
			if !perEvent {
				label = fmt.Sprintf("gzip-%d flush at end", level)
			}
			fmt.Printf("%-34s %12d %8.2fx %10.1f\n", label, n, float64(base)/float64(n), float64(ms.Microseconds())/1000)
		}
	}
	fmt.Println()
}

// TestCorpusPerEventFlushOverhead isolates the cost of Z_SYNC_FLUSH per
// event: same bytes, same level, only the flush cadence differs.
func TestCorpusPerEventFlushOverhead(t *testing.T) {
	events := loadCorpus(t)
	perEvent, _ := serveAndCount(t, events, DefaultLevel, true, true)
	batched, _ := serveAndCount(t, events, DefaultLevel, false, true)
	overhead := perEvent - batched
	fmt.Printf("\n=== per-event flush overhead (level %d, %d events) ===\n", DefaultLevel, len(events))
	fmt.Printf("flush per event : %d B\nflush at end    : %d B\noverhead        : %d B total, %.1f B/event, %.2f%%\n\n",
		perEvent, batched, overhead, float64(overhead)/float64(len(events)), 100*float64(overhead)/float64(batched))
}

// TestCorpusSmallEventCost measures the frames that dominate event *count*
// rather than bytes: session-activity pings (~85 B each). A tiny event in a
// long-lived stream compresses against the stream dictionary.
func TestCorpusSmallEventCost(t *testing.T) {
	var pings []sseEvent
	for i := 0; i < 200; i++ {
		pings = append(pings, sseEvent{name: "session-activity", data: []byte(`{"type":"session-activity","id":"01K4Z8QK3H7V2M9T5PJX6RW0AB"}`)})
	}
	raw := rawSize(pings)
	n, _ := serveAndCount(t, pings, DefaultLevel, true, true)
	fmt.Printf("\n=== 200 session-activity pings, flush per event ===\nraw %d B (%.0f B/event) -> gzip %d B (%.0f B/event), %.2fx\n\n",
		raw, float64(raw)/200, n, float64(n)/200, float64(raw)/float64(n))
}

// TestCorpusConcurrentSubscribers is the N=10 daemon-cost measurement: ten
// subscribers each receiving one full bootstrap, as a broadcast would.
func TestCorpusConcurrentSubscribers(t *testing.T) {
	events := loadCorpus(t)
	for _, n := range []int{1, 10} {
		for _, gzipOn := range []bool{false, true} {
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeEvents(w, events, true) })
			var handler http.Handler = h
			if gzipOn {
				handler = Gzip(h)
			}
			srv := httptest.NewServer(handler)
			cpu0 := processCPU()
			start := time.Now()
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
					if gzipOn {
						req.Header.Set("Accept-Encoding", "gzip")
					} else {
						req.Header.Set("Accept-Encoding", "identity")
					}
					resp, err := http.DefaultTransport.RoundTrip(req)
					if err != nil {
						t.Error(err)
						return
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}()
			}
			wg.Wait()
			wall := time.Since(start)
			cpu := processCPU() - cpu0
			srv.Close()
			fmt.Printf("N=%-3d gzip=%-5v wall %7.1f ms  process CPU %7.1f ms (%.1f ms/subscriber, includes client-side read)\n",
				n, gzipOn, float64(wall.Microseconds())/1000, float64(cpu.Microseconds())/1000, float64(cpu.Microseconds())/1000/float64(n))
		}
	}
	fmt.Println()
}

// BenchmarkCorpusEncode attributes CPU to the server side only: compress
// the corpus with flush-per-event into io.Discard.
func BenchmarkCorpusEncode(b *testing.B) {
	events := loadCorpus(b)
	for _, level := range []int{1, 5, 6, 7, 8, 9} {
		b.Run(fmt.Sprintf("level%d", level), func(b *testing.B) {
			b.SetBytes(int64(rawSize(events)))
			for i := 0; i < b.N; i++ {
				zw, _ := gzip.NewWriterLevel(io.Discard, level)
				for _, e := range events {
					fmt.Fprintf(zw, "event: %s\ndata: %s\n\n", e.name, e.data)
					_ = zw.Flush()
				}
				zw.Close()
			}
		})
	}
}

// TestCorpusRepeatedResends measures what protocol 3's full re-sends cost on
// a *connection-lived* compressor: the second and later copies of a nearly
// identical world compress against the first. This is the foreground-churn
// number (13 re-sends per 25 minutes on the operator's corpus).
func TestCorpusRepeatedResends(t *testing.T) {
	events := loadCorpus(t)
	const n = 6
	var marks []int
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < n; i++ {
			writeEvents(w, events, true)
		}
	})
	srv := httptest.NewServer(Gzip(h))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Count wire bytes per decoded copy by reading the compressed stream
	// through a counting reader wrapped by the gzip decoder.
	cr := &countingReader{r: resp.Body}
	zr, err := gzip.NewReader(cr)
	if err != nil {
		t.Fatal(err)
	}
	one := rawSize(events)
	buf := make([]byte, 32*1024)
	decoded, copies := 0, 0
	for copies < n {
		k, err := zr.Read(buf)
		decoded += k
		for copies < n && decoded >= (copies+1)*one {
			marks = append(marks, cr.n)
			copies++
		}
		if err != nil {
			break
		}
	}
	fmt.Printf("\n=== identical full re-sends on one connection (%d copies of the %d-byte bootstrap) ===\n", n, one)
	prev := 0
	for i, m := range marks {
		fmt.Printf("copy %d: cumulative wire %8d B   this copy %8d B (%.0fx vs raw)\n", i+1, m, m-prev, float64(one)/float64(m-prev))
		prev = m
	}
	fmt.Println()
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += k
	return k, err
}
