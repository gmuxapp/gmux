package httpz

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The benchmarks replay an SSE stream through the middleware with the
// daemon's per-event flush discipline (sendSSETransaction: one write, one
// Flush per event) and report wire bytes per level. They exist to re-decide
// DefaultLevel against a real corpus:
//
//	curl -sS --unix-socket ~/.local/state/gmux/gmuxd.sock --max-time 20 \
//	  'http://d/v1/events?session_stream=3' -o /tmp/boot.sse
//	GMUX_SSE_CORPUS=/tmp/boot.sse go test ./internal/httpz -run xxx -bench Level -benchtime 5x
//
// Without the env var they run on a synthetic protocol-3-shaped stream, so
// the code path stays exercised in CI without shipping a corpus.

type sseEvent struct {
	name string
	data []byte
}

func loadCorpus(tb testing.TB) []sseEvent {
	tb.Helper()
	path := os.Getenv("GMUX_SSE_CORPUS")
	if path == "" {
		return syntheticCorpus()
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

// syntheticCorpus approximates one protocol-3 bootstrap: a begin marker,
// batches of ~40 JSON session rows with repetitive keys and semi-unique
// values, a ready marker, then a handful of tiny activity pings.
func syntheticCorpus() []sseEvent {
	var events []sseEvent
	events = append(events, sseEvent{"snapshot.sessions.begin", []byte(`{"epoch":1,"total":400}`)})
	for b := 0; b < 10; b++ {
		var sb strings.Builder
		sb.WriteString(`{"epoch":1,"rows":[`)
		for i := 0; i < 40; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"id":"s%04d","host":"desktop","adapter":"pi","alive":%v,"title":"task %d of many","cwd":"/home/u/dev/proj%d","preview":"%s","last_output_at":"2026-09-14T10:%02d:00Z"}`,
				b*40+i, i%3 == 0, i, b, strings.Repeat("output line ", 20), i)
		}
		sb.WriteString(`]}`)
		events = append(events, sseEvent{"snapshot.sessions.batch", []byte(sb.String())})
	}
	events = append(events, sseEvent{"snapshot.sessions.ready", []byte(`{"epoch":1}`)})
	for i := 0; i < 20; i++ {
		events = append(events, sseEvent{"session-activity", []byte(fmt.Sprintf(`{"type":"session-activity","id":"s%04d"}`, i))})
	}
	return events
}

func rawSize(events []sseEvent) int {
	n := 0
	for _, e := range events {
		n += len("event: ") + len(e.name) + len("\ndata: ") + len(e.data) + len("\n\n")
	}
	return n
}

// replay serves one subscriber through the middleware at the given level and
// returns the bytes that crossed the socket.
func replay(tb testing.TB, events []sseEvent, level int) int {
	tb.Helper()
	srv := httptest.NewServer(GzipLevel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for _, e := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
			_ = rc.Flush()
		}
	}), level))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		tb.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		tb.Fatal(err)
	}
	return int(n)
}

func BenchmarkLevel(b *testing.B) {
	events := loadCorpus(b)
	raw := rawSize(events)
	for _, level := range []int{1, 5, 6, 7, 9} {
		b.Run(fmt.Sprintf("L%d", level), func(b *testing.B) {
			var wire int
			b.SetBytes(int64(raw))
			for i := 0; i < b.N; i++ {
				wire = replay(b, events, level)
			}
			b.ReportMetric(float64(wire), "wire-bytes")
			b.ReportMetric(float64(raw)/float64(wire), "ratio")
		})
	}
}
