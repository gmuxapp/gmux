package netauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestIntegrationFullLoginFlow exercises the complete browser login flow:
// unauthenticated request → redirect → login page → POST token → cookie → access.
func TestIntegrationFullLoginFlow(t *testing.T) {
	handler := Middleware(testToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("protected content"))
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{
		// Don't follow redirects automatically; we want to inspect each step.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Step 1: GET / without auth → redirect to /auth/login.
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("step 1: status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/auth/login" {
		t.Fatalf("step 1: Location = %q, want /auth/login", loc)
	}

	// Step 2: GET /auth/login → login page HTML.
	resp, err = client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 2: status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Access Token") {
		t.Fatal("step 2: login page missing token input")
	}

	// Step 3: POST /auth/login with valid token → redirect + cookie.
	form := url.Values{"token": {testToken}}
	resp, err = client.PostForm(srv.URL+"/auth/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("step 3: status = %d, want 303", resp.StatusCode)
	}

	// Extract the cookie.
	var authCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			authCookie = c
			break
		}
	}
	if authCookie == nil {
		t.Fatal("step 3: no auth cookie set")
	}

	// Step 4: GET / with cookie → protected content.
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.AddCookie(authCookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4: status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "protected content" {
		t.Fatalf("step 4: body = %q, want %q", string(body), "protected content")
	}
}

// TestIntegrationBearerTokenAPI tests programmatic access with bearer token.
func TestIntegrationBearerTokenAPI(t *testing.T) {
	handler := Middleware(testToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Without token → 401.
	resp, err := http.Get(srv.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", resp.StatusCode)
	}

	// With bearer token → 200.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer: status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("bearer: body = %q", string(body))
	}
}

// TestIntegrationQRCodeFlow tests the QR code link flow where the token
// is in the URL query parameter.
func TestIntegrationQRCodeFlow(t *testing.T) {
	handler := Middleware(testToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("home"))
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// GET /auth/login?token=<valid> → redirect to / with cookie.
	resp, err := client.Get(srv.URL + "/auth/login?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	var authCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			authCookie = c
			break
		}
	}
	if authCookie == nil {
		t.Fatal("no cookie set via QR code flow")
	}

	// Follow up with the cookie to verify access.
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.AddCookie(authCookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after QR: status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "home" {
		t.Fatalf("after QR: body = %q", string(body))
	}
}

// TestIntegrationShutdownBlockedOnNetworkListener verifies that the middleware
// blocks /v1/shutdown entirely, even with valid auth. Shutdown is a localhost-only
// operation; the network listener should never expose it.
func TestIntegrationShutdownBlockedOnNetworkListener(t *testing.T) {
	shutdownCalled := false
	inner := http.NewServeMux()
	inner.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		shutdownCalled = true
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(testToken, inner)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if shutdownCalled {
		t.Fatal("shutdown handler should NOT have been called; middleware should block it")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}
