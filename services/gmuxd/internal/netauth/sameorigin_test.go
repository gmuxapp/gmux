package netauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// cookieReq builds a cookie-authenticated request against the given
// method/target with the given request Host.
func cookieReq(method, target, host string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	return req
}

func serve(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	h := Middleware(testToken, okHandler())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// ── Mutating /v1/ requests, cookie-authed ──

func TestCookiePostSameOriginAllowed(t *testing.T) {
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Origin", "http://gmux.example.com")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestCookiePostCrossOriginBlocked(t *testing.T) {
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Origin", "https://evil.example.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestCookiePostSameSiteSiblingBlocked(t *testing.T) {
	// The ts.net co-tenant case: another service on the same tailnet is
	// same-SITE (ts.net is on the PSL) but not same-ORIGIN. SameSite=Strict
	// attaches the cookie; the middleware must still reject.
	req := cookieReq("POST", "/v1/sessions", "gmux-host.tailnet.ts.net")
	req.Header.Set("Origin", "https://other-app.tailnet.ts.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for same-site sibling origin", rr.Code)
	}
}

func TestCookiePostSecFetchSiteSameOriginAllowed(t *testing.T) {
	// Sec-Fetch-Site alone suffices (Origin may be absent on some paths).
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestCookiePostSecFetchSiteSameSiteBlocked(t *testing.T) {
	// Even if Origin were spoofed to match, an explicit same-site
	// attestation from the browser wins.
	req := cookieReq("POST", "/v1/sessions", "gmux-host.tailnet.ts.net")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Origin", "https://gmux-host.tailnet.ts.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when Sec-Fetch-Site says same-site", rr.Code)
	}
}

func TestCookiePostMissingHeadersBlocked(t *testing.T) {
	// A cookie-authed mutating request with neither Sec-Fetch-Site nor
	// Origin cannot prove same-origin. Browsers always send at least
	// Origin on non-GET; header-less mutation is curl-with-a-cookie,
	// which should use a bearer token instead.
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestCookiePostNullOriginBlocked(t *testing.T) {
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Origin", "null")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for Origin: null", rr.Code)
	}
}

func TestCookieDeleteCrossOriginBlocked(t *testing.T) {
	req := cookieReq("DELETE", "/v1/sessions/abc", "gmux.example.com")
	req.Header.Set("Origin", "https://other.example.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// ── GET reads, cookie-authed: exempt ──

func TestCookieGetWithoutOriginAllowed(t *testing.T) {
	req := cookieReq("GET", "/v1/sessions", "gmux.example.com")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (GET reads are exempt)", rr.Code)
	}
}

func TestCookieGetCrossOriginAllowed(t *testing.T) {
	// Cross-origin GETs are harmless without CORS headers: the foreign
	// page cannot read the response. Blocking them would only break
	// things like proxies that strip fetch metadata.
	req := cookieReq("GET", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Origin", "https://other.example.net")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// ── WebSocket upgrades, cookie-authed ──

func TestCookieWSUpgradeSameOriginAllowed(t *testing.T) {
	req := cookieReq("GET", "/ws/session-1", "gmux-host.tailnet.ts.net")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "https://gmux-host.tailnet.ts.net")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestCookieWSUpgradeCrossOriginBlocked(t *testing.T) {
	// WS upgrades are GETs but must still be origin-checked: a foreign
	// page CAN read a hijacked WebSocket (CSWSH), and /ws is the PTY.
	req := cookieReq("GET", "/ws/session-1", "gmux-host.tailnet.ts.net")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "https://other-app.tailnet.ts.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for cross-origin WS hijack", rr.Code)
	}
}

func TestCookieWSUpgradeNoOriginBlocked(t *testing.T) {
	req := cookieReq("GET", "/ws/session-1", "gmux.example.com")
	req.Header.Set("Upgrade", "websocket")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (browsers always send Origin on WS)", rr.Code)
	}
}

// ── Bearer auth: fully exempt from origin constraints ──

func TestBearerPostCrossOriginAllowed(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/sessions", nil)
	req.Host = "gmux.example.com"
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://anywhere.example.net")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (bearer requests carry no origin constraint)", rr.Code)
	}
}

func TestBearerPrecedenceOverCookie(t *testing.T) {
	// When both credentials are present and the bearer is valid, the
	// request authenticated explicitly — no origin constraint applies,
	// even if the browser also attached the ambient cookie.
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Authorization", "Bearer "+testToken)
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (valid bearer wins over cookie)", rr.Code)
	}
}

func TestInvalidBearerFallsBackToCookieConstraints(t *testing.T) {
	// A wrong bearer must not grant the bearer exemption: auth falls back
	// to the cookie, and the cookie's same-origin rules apply.
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Origin", "https://evil.example.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (invalid bearer must not lift origin constraint)", rr.Code)
	}
}

func TestBearerWSUpgradeNoOriginAllowed(t *testing.T) {
	// Hub→spoke WS proxying and CLI clients dial with a bearer header and
	// no Origin. They must keep working.
	req := httptest.NewRequest("GET", "/ws/session-1", nil)
	req.Host = "spoke.tailnet.ts.net"
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Upgrade", "websocket")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (bearer WS must not require Origin)", rr.Code)
	}
}

// ── Reverse proxy shapes ──

func TestCookiePostBehindProxyMatchesForwardedHost(t *testing.T) {
	// A proxy that rewrites Host to the upstream address must still pass:
	// the browser-facing host arrives in X-Forwarded-Host.
	req := cookieReq("POST", "/v1/sessions", "127.0.0.1:8790")
	req.Header.Set("Origin", "https://gmux.example.com")
	req.Header.Set("X-Forwarded-Host", "gmux.example.com")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (X-Forwarded-Host must be honored)", rr.Code)
	}
}

func TestCookiePostBehindProxyPreservedHost(t *testing.T) {
	// Traefik default: Host is forwarded verbatim, no X-Forwarded-Host
	// needed. Origin is https, upstream hop is plain http — the check
	// must compare hosts, not schemes.
	req := cookieReq("POST", "/v1/sessions", "gmux.example.com")
	req.Header.Set("Origin", "https://gmux.example.com")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestCookiePostSpoofedForwardedHostBlockedByFetchMetadata(t *testing.T) {
	// The X-Forwarded-Host spoof shape: a hostile page fetch()es with
	// credentials and sets X-Forwarded-Host to its own origin's host so
	// that Origin == XFH "matches". Any browser capable of attaching that
	// custom header sends Sec-Fetch-Site, which must win over the header
	// comparison. (The other layer — the CORS preflight such a request
	// triggers — can't be expressed in a unit test.)
	req := cookieReq("POST", "/v1/sessions", "gmux-host.tailnet.ts.net")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Origin", "https://evil.tailnet.ts.net")
	req.Header.Set("X-Forwarded-Host", "evil.tailnet.ts.net")
	if rr := serve(t, req); rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (Sec-Fetch-Site must override spoofed X-Forwarded-Host)", rr.Code)
	}
}

func TestForwardedHostChainUsesFirstHop(t *testing.T) {
	req := cookieReq("POST", "/v1/sessions", "127.0.0.1:8790")
	req.Header.Set("Origin", "https://gmux.example.com")
	req.Header.Set("X-Forwarded-Host", "gmux.example.com, 10.0.0.5:8080")
	if rr := serve(t, req); rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (first hop of X-Forwarded-Host chain)", rr.Code)
	}
}

func TestHostsMatchDefaultPorts(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"gmux.example.com", "gmux.example.com", true},
		{"GMUX.Example.com", "gmux.example.com", true},
		{"gmux.example.com:443", "gmux.example.com", true},
		{"gmux.example.com:80", "gmux.example.com", true},
		{"localhost:8790", "localhost:8790", true},
		{"localhost:8790", "localhost:3000", false},
		{"a.tailnet.ts.net", "b.tailnet.ts.net", false},
	}
	for _, c := range cases {
		if got := hostsMatch(c.a, c.b); got != c.want {
			t.Errorf("hostsMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// ── Response headers & cookie attributes ──

func TestFrameEmbeddingDenied(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	rr := serve(t, req)
	if got := rr.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("CSP = %q, want frame-ancestors 'none'", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}

func TestCookieSecureOverTLS(t *testing.T) {
	req := httptest.NewRequest("GET", "https://gmux.example.com/auth/login?token="+testToken, nil)
	rr := serve(t, req)
	assertCookieSecure(t, rr, true)
}

func TestCookieSecureBehindTLSProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/auth/login?token="+testToken, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := serve(t, req)
	assertCookieSecure(t, rr, true)
}

func TestCookieSecureBehindChainedTLSProxy(t *testing.T) {
	// Multiple proxy hops append to X-Forwarded-Proto; the browser-facing
	// hop is the first element.
	req := httptest.NewRequest("GET", "/auth/login?token="+testToken, nil)
	req.Header.Set("X-Forwarded-Proto", "https, http")
	rr := serve(t, req)
	assertCookieSecure(t, rr, true)
}

func TestCookieNotSecureOverPlainHTTP(t *testing.T) {
	// Plain-http localhost/LAN login must not set Secure, or the browser
	// discards the cookie and login loops forever.
	req := httptest.NewRequest("GET", "/auth/login?token="+testToken, nil)
	rr := serve(t, req)
	assertCookieSecure(t, rr, false)
}

func assertCookieSecure(t *testing.T, rr *httptest.ResponseRecorder, want bool) {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			if c.Secure != want {
				t.Errorf("cookie Secure = %v, want %v", c.Secure, want)
			}
			return
		}
	}
	t.Fatal("expected auth cookie to be set")
}
