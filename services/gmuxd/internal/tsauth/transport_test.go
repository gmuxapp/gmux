package tsauth

import (
	"net/http"
	"net/url"
	"testing"
)

type stubRT struct{ name string }

func (s *stubRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

func reqFor(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return &http.Request{URL: u}
}

// A RoutedTransport with no tailnet configured must send everything —
// including .ts.net hosts — through the fallback, so behavior before
// tsnet is ready matches the pre-#281 default-transport path.
func TestRoutedTransport_FallbackBeforeReady(t *testing.T) {
	fallback := &stubRT{name: "fallback"}
	rt := &RoutedTransport{fallback: fallback}

	for _, u := range []string{
		"https://gmux.tailnet.ts.net",
		"http://192.168.1.10:9999",
		"http://localhost:9999",
	} {
		if got := rt.pick(reqFor(t, u).URL.Hostname()); got != fallback {
			t.Errorf("pick(%s) before SetTailnet: got tsnet route, want fallback", u)
		}
	}
}

// After SetTailnet, only hosts under the local tailnet's MagicDNS suffix
// route through tsnet; raw IPs, LAN names, and hosts on a *different*
// tailnet keep the default transport (issue #281 acceptance).
func TestRoutedTransport_SuffixRouting(t *testing.T) {
	fallback := &stubRT{name: "fallback"}
	ts := &stubRT{name: "tsnet"}
	rt := &RoutedTransport{fallback: fallback}
	rt.SetTailnet("tailnet.ts.net", ts)

	cases := []struct {
		url    string
		wantTS bool
	}{
		{"https://gmux.tailnet.ts.net", true},
		{"https://gmux.tailnet.ts.net:443", true},
		{"https://GMUX.TAILNET.TS.NET", true},     // case-insensitive
		{"https://gmux.tailnet.ts.net./v1", true}, // trailing dot
		{"https://other.othernet.ts.net", false},  // different tailnet
		{"https://evil-tailnet.ts.net", false},    // suffix must match on a label boundary
		{"http://192.168.1.10:9999", false},
		{"http://localhost:9999", false},
		{"http://nas.lan:9999", false},
	}
	for _, c := range cases {
		got := rt.pick(reqFor(t, c.url).URL.Hostname())
		if (got == ts) != c.wantTS {
			t.Errorf("pick(%s): tsnet=%v, want %v", c.url, got == ts, c.wantTS)
		}
	}
}

// SetTailnet normalizes the suffix so a trailing-dot value from
// tailscale's Status (e.g. "tailnet.ts.net.") still matches.
func TestRoutedTransport_NormalizesSuffix(t *testing.T) {
	ts := &stubRT{name: "tsnet"}
	rt := NewRoutedTransport()
	rt.SetTailnet("Tailnet.ts.net.", ts)

	if got := rt.pick("gmux.tailnet.ts.net"); got != ts {
		t.Errorf("suffix with trailing dot/case: got fallback, want tsnet")
	}
}
