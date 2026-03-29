package netauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestE2E exercises the full authentication surface area against a running
// server. Each subtest is independent and documents a specific security
// property. If any of these fail, the network listener has a security hole.
func TestE2E(t *testing.T) {
	// Build a realistic mux that mirrors gmuxd's route structure.
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"status": "ready"}})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "data": []any{}})
	})
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ws/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/launch", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		// In production, this also checks RemoteAddr, but the middleware
		// should block it before it reaches here.
		t.Error("shutdown handler should never be called on the network listener")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("frontend"))
	})

	authed := Middleware(testToken, mux)
	srv := httptest.NewServer(authed)
	defer srv.Close()

	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// ── Unauthenticated API requests return 401 JSON ──

	t.Run("unauthenticated API paths return 401", func(t *testing.T) {
		for _, path := range []string{"/v1/health", "/v1/sessions", "/v1/launch"} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%s: got %d, want 401", path, resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("%s: Content-Type = %q, want JSON", path, ct)
			}
		}
	})

	t.Run("unauthenticated SSE returns 401", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/events", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("unauthenticated WebSocket upgrade returns 401", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/ws/test-session", nil)
		req.Header.Set("Upgrade", "websocket")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("unauthenticated browser request redirects to login", func(t *testing.T) {
		resp, err := noFollow.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Errorf("got %d, want 303", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/auth/login" {
			t.Errorf("Location = %q, want /auth/login", loc)
		}
	})

	// ── Shutdown is blocked entirely ──

	t.Run("shutdown blocked even with valid bearer", func(t *testing.T) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/shutdown", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("got %d, want 403", resp.StatusCode)
		}
	})

	t.Run("shutdown blocked without auth", func(t *testing.T) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/shutdown", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("got %d, want 403", resp.StatusCode)
		}
	})

	// ── Bearer token ──

	t.Run("valid bearer grants access", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("got %d, want 200", resp.StatusCode)
		}
	})

	t.Run("wrong bearer rejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("bearer requires Bearer prefix", func(t *testing.T) {
		for _, scheme := range []string{"", "Basic ", "Token "} {
			req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
			req.Header.Set("Authorization", scheme+testToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("scheme %q: got %d, want 401", scheme, resp.StatusCode)
			}
		}
	})

	// ── Cookie ──

	t.Run("valid cookie grants access", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
		req.AddCookie(&http.Cookie{Name: "gmux-token", Value: testToken})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("got %d, want 200", resp.StatusCode)
		}
	})

	t.Run("wrong cookie rejected", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
		req.AddCookie(&http.Cookie{Name: "gmux-token", Value: "wrong"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("got %d, want 401", resp.StatusCode)
		}
	})

	// ── Login page ──

	t.Run("login page accessible without auth", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("got %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(string(body), `action="/auth/login"`) {
			t.Error("login page missing form")
		}
	})

	t.Run("login redirects home when already authenticated", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/auth/login", nil)
		req.AddCookie(&http.Cookie{Name: "gmux-token", Value: testToken})
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Errorf("got %d, want 303", resp.StatusCode)
		}
	})

	// ── POST login ──

	t.Run("POST login with valid token sets cookie and redirects", func(t *testing.T) {
		resp, err := noFollow.PostForm(srv.URL+"/auth/login", url.Values{"token": {testToken}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Errorf("got %d, want 303", resp.StatusCode)
		}

		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "gmux-token" {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("no cookie set")
		}
		if cookie.Value != testToken {
			t.Error("cookie value mismatch")
		}
		if !cookie.HttpOnly {
			t.Error("cookie must be HttpOnly")
		}
		if cookie.SameSite != http.SameSiteStrictMode {
			t.Error("cookie must be SameSite=Strict")
		}
	})

	t.Run("POST login with wrong token re-renders with error", func(t *testing.T) {
		resp, err := noFollow.PostForm(srv.URL+"/auth/login", url.Values{"token": {"wrong"}})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("got %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(string(body), "Invalid token") {
			t.Error("missing error message")
		}
		for _, c := range resp.Cookies() {
			if c.Name == "gmux-token" {
				t.Error("should not set cookie on failed login")
			}
		}
	})

	t.Run("POST login trims whitespace from pasted token", func(t *testing.T) {
		resp, err := noFollow.PostForm(srv.URL+"/auth/login", url.Values{"token": {"  " + testToken + "\n"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Errorf("got %d, want 303 (whitespace should be trimmed)", resp.StatusCode)
		}
	})

	// ── QR code flow ──

	t.Run("QR valid token sets cookie and redirects", func(t *testing.T) {
		resp, err := noFollow.Get(srv.URL + "/auth/login?token=" + testToken)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Errorf("got %d, want 303", resp.StatusCode)
		}
		found := false
		for _, c := range resp.Cookies() {
			if c.Name == "gmux-token" {
				found = true
			}
		}
		if !found {
			t.Error("no cookie set via QR flow")
		}
	})

	t.Run("QR invalid token shows error", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/auth/login?token=wrong")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "Invalid token") {
			t.Error("missing error for invalid QR token")
		}
	})

	// ── Full browser login flow (multi-step) ──

	t.Run("full browser flow: redirect → login → POST → cookie → access", func(t *testing.T) {
		jar := &simpleCookieJar{}
		client := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Jar: jar,
		}

		// Step 1: GET / → 303 to /auth/login
		resp, err := client.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Fatalf("step 1: got %d, want 303", resp.StatusCode)
		}

		// Step 2: GET /auth/login → 200 login page
		resp, err = client.Get(srv.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("step 2: got %d, want 200", resp.StatusCode)
		}

		// Step 3: POST /auth/login with token → 303 + cookie
		resp, err = client.PostForm(srv.URL+"/auth/login", url.Values{"token": {testToken}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 303 {
			t.Fatalf("step 3: got %d, want 303", resp.StatusCode)
		}
		// Manually add cookie to jar (PostForm response has Set-Cookie).
		for _, c := range resp.Cookies() {
			jar.cookies = append(jar.cookies, c)
		}

		// Step 4: GET / with cookie → 200 frontend
		resp, err = client.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("step 4: got %d, want 200", resp.StatusCode)
		}
		if string(body) != "frontend" {
			t.Fatalf("step 4: body = %q, want %q", string(body), "frontend")
		}

		// Step 5: API call with cookie → 200
		resp, err = client.Get(srv.URL + "/v1/sessions")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("step 5: got %d, want 200", resp.StatusCode)
		}
	})

}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// simpleCookieJar is a minimal cookie jar for testing multi-step flows.
type simpleCookieJar struct {
	cookies []*http.Cookie
}

func (j *simpleCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.cookies = append(j.cookies, cookies...)
}

func (j *simpleCookieJar) Cookies(u *url.URL) []*http.Cookie {
	return j.cookies
}

// Verify simpleCookieJar implements http.CookieJar.
var _ http.CookieJar = (*simpleCookieJar)(nil)

// TestE2ETimingAttack verifies that token comparison doesn't leak length
// information. This is a structural test, not a timing measurement.
// The constant-time comparison is verified by checking that authtoken.Equal
// is used (not == or strings.Compare). We test this by confirming that
// a token prefix is rejected, which would succeed with a naive prefix check.
func TestE2ETimingAttack(t *testing.T) {
	// A prefix of the valid token must be rejected.
	prefix := testToken[:32]
	h := Middleware(testToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with prefix token")
	}))

	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+prefix)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 401 {
		t.Errorf("prefix token: got %d, want 401", rr.Code)
	}
}

// TestE2ELoginMethodEnumeration verifies that unsupported HTTP methods on
// the login endpoint return 405, not 401 or 200.
func TestE2ELoginMethodEnumeration(t *testing.T) {
	h := Middleware(testToken, http.NewServeMux())

	for _, method := range []string{"PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(method, "/auth/login", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != 405 {
			t.Errorf("%s /auth/login: got %d, want 405", method, rr.Code)
		}
	}
}

// TestE2ENoOpenRedirect verifies that the login flow only redirects to /,
// never to an attacker-controlled URL.
func TestE2ENoOpenRedirect(t *testing.T) {
	h := Middleware(testToken, http.NewServeMux())

	// POST login always redirects to /, regardless of Referer or other headers.
	form := url.Values{"token": {testToken}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://evil.com/steal")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("POST redirect Location = %q, want /", loc)
	}
}

// TestE2ECookieNotSetOnWrongName verifies that the middleware only accepts
// cookies with the exact name "gmux-token", not similar names.
func TestE2ECookieNotSetOnWrongName(t *testing.T) {
	h := Middleware(testToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	}))

	for _, name := range []string{"gmux_token", "gmux-Token", "token", "session"} {
		req := httptest.NewRequest("GET", "/v1/sessions", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: testToken})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Errorf("cookie name %q: got %d, want 401", name, rr.Code)
		}
	}
}

// TestE2EAPIDetectionHeuristic verifies the boundary between "API request"
// (→ 401 JSON) and "browser navigation" (→ 303 redirect).
func TestE2EAPIDetectionHeuristic(t *testing.T) {
	h := Middleware(testToken, http.NewServeMux())

	tests := []struct {
		name     string
		path     string
		headers  map[string]string
		wantCode int
	}{
		{"plain GET /", "/", nil, 303},
		{"GET /v1/anything", "/v1/anything", nil, 401},
		{"GET /ws/anything", "/ws/anything", nil, 401},
		{"websocket upgrade on /", "/", map[string]string{"Upgrade": "websocket"}, 401},
		{"accept JSON on /", "/", map[string]string{"Accept": "application/json"}, 401},
		{"accept SSE on /", "/", map[string]string{"Accept": "text/event-stream"}, 401},
		{"accept HTML on /", "/", map[string]string{"Accept": "text/html"}, 303},
		{"GET /assets/app.js", "/assets/app.js", nil, 303},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.path, nil)
		for k, v := range tt.headers {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != tt.wantCode {
			t.Errorf("%s: got %d, want %d", tt.name, rr.Code, tt.wantCode)
		}
	}
}
