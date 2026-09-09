package update

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v0.5.0", "v0.4.6", true},
		{"v0.4.7", "v0.4.6", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.4.6", "v0.4.6", false},
		{"v0.4.5", "v0.4.6", false},
		{"v0.3.0", "v0.4.6", false},
		{"0.5.0", "0.4.6", true}, // no prefix
		{"dev", "v0.4.6", false}, // unparseable
		{"v0.4.6", "dev", false}, // unparseable
	}
	for _, tt := range tests {
		got := newer(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"1.2.3", true},
		{"0.0.0", true},
		{"1.2", false},
		{"abc", false},
		{"1.2.3-beta", false},
	}
	for _, tt := range tests {
		got := parseSemver(tt.in)
		if (got != nil) != tt.want {
			t.Errorf("parseSemver(%q): got nil=%v, want valid=%v", tt.in, got == nil, tt.want)
		}
	}
}

// recordingTransport answers the releases API and records every request, so a
// test can assert on "did this checker touch the network at all".
type recordingTransport struct {
	tag string

	mu   sync.Mutex
	urls []string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.urls = append(rt.urls, req.URL.String())
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"tag_name":%q}`, rt.tag))),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (rt *recordingTransport) requests() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.urls...)
}

// loopGoroutines counts running Checker.loop goroutines. The dev-build
// contract is "no goroutine", and a lingering loop from an earlier test in
// this binary makes an absolute count useless, so callers compare a delta.
func loopGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "update.(*Checker).loop(")
}

// The update checker must stay offline — not merely quiet — for every build
// that came from a working tree, including the stamped "dev+<hash>" a source
// install produces (scripts/build.sh). "Quiet" is not the property worth
// pinning: Available() is "" for any version until a check succeeds, so
// asserting only that says nothing.
func TestDevBuildsNeverCheckForUpdates(t *testing.T) {
	// The predicate itself lives in packages/buildversion (one copy for the
	// three policies that key off it); pin the behaviour here.
	for _, v := range []string{"dev", "dev+1a2b3c4d5e6f", "dev+1a2b3c4d5e6f-dirty"} {
		t.Run(v, func(t *testing.T) {
			transport := &recordingTransport{tag: "v99.0.0"}
			before := loopGoroutines()
			c := newChecker(v, transport)

			// A started loop checks immediately, so a request would already be
			// recorded; the wait only buys margin on a loaded machine.
			time.Sleep(200 * time.Millisecond)

			if got := transport.requests(); len(got) != 0 {
				t.Errorf("dev build %q went to the network: %v", v, got)
			}
			if after := loopGoroutines(); after != before {
				t.Errorf("dev build %q started the checker goroutine (loops %d → %d)", v, before, after)
			}
			if c.Available() != "" {
				t.Errorf("dev build %q reported an available update: %q", v, c.Available())
			}
		})
	}
}

// The mirror image, so the dev branch cannot pass by never checking at all: a
// release build does poll, against the releases endpoint, and adopts a newer
// tag.
func TestReleaseBuildsCheckForUpdates(t *testing.T) {
	transport := &recordingTransport{tag: "v9.9.9"}
	c := newChecker("v2.1.0", transport)

	deadline := time.Now().Add(5 * time.Second)
	for c.Available() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := c.Available(); got != "v9.9.9" {
		t.Fatalf("release build did not pick up the newer release: Available() = %q", got)
	}
	requests := transport.requests()
	if len(requests) == 0 {
		t.Fatal("release build never contacted the releases API")
	}
	if want := "https://api.github.com/repos/" + repo + "/releases/latest"; requests[0] != want {
		t.Errorf("checked %q, want %q", requests[0], want)
	}
}
