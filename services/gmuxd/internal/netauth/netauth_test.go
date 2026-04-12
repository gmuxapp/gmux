package netauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestBearerTokenAccepted(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rr.Body.String(), "ok")
	}
}

func TestBearerTokenRejected(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestMissingAuthOnAPIReturns401(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestMissingAuthOnBrowserRedirectsToLogin(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/auth/login" {
		t.Errorf("Location = %q, want /auth/login", loc)
	}
}

func TestCookieAccepted(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestCookieRejectedWrongValue(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "wrong"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestLoginPageServedWithoutAuth(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/auth/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), "Access Token") {
		t.Error("login page should contain token input")
	}
}

func TestLoginPageRedirectsWhenAuthenticated(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/auth/login", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 redirect to /", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

func TestLoginPostValidToken(t *testing.T) {
	h := Middleware(testToken, okHandler())

	form := url.Values{"token": {testToken}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}

	// Check that the cookie was set.
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == cookieName {
			found = true
			if c.Value != testToken {
				t.Errorf("cookie value = %q, want token", c.Value)
			}
			if !c.HttpOnly {
				t.Error("cookie should be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie SameSite = %v, want Strict", c.SameSite)
			}
			if c.MaxAge != cookieMaxAge {
				t.Errorf("cookie MaxAge = %d, want %d", c.MaxAge, cookieMaxAge)
			}
		}
	}
	if !found {
		t.Error("expected cookie to be set")
	}
}

func TestLoginPostInvalidToken(t *testing.T) {
	h := Middleware(testToken, okHandler())

	form := url.Values{"token": {"wrong-token"}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-render login with error)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid token") {
		t.Error("should show error message for invalid token")
	}

	// No cookie should be set.
	cookies := rr.Result().Cookies()
	for _, c := range cookies {
		if c.Name == cookieName {
			t.Error("should not set cookie on invalid token")
		}
	}
}

func TestLoginPostEmptyToken(t *testing.T) {
	h := Middleware(testToken, okHandler())

	form := url.Values{"token": {""}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid token") {
		t.Error("should show error for empty token")
	}
}

func TestWebSocketUpgradeWithoutAuth(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/ws/test-session", nil)
	req.Header.Set("Upgrade", "websocket")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for unauthenticated websocket", rr.Code)
	}
}

func TestSSEWithoutAuth(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for unauthenticated SSE", rr.Code)
	}
}

func TestBearerWithoutPrefixRejected(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	// Set the token directly without "Bearer " prefix.
	req.Header.Set("Authorization", testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for auth without Bearer prefix", rr.Code)
	}
}

func TestLoginMethodNotAllowed(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("DELETE", "/auth/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestLoginGetWithValidURLToken(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/auth/login?token="+testToken, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}

	// Cookie should be set.
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == cookieName {
			found = true
			if c.Value != testToken {
				t.Errorf("cookie value = %q", c.Value)
			}
		}
	}
	if !found {
		t.Error("expected cookie to be set via URL token")
	}
}

func TestLoginGetWithInvalidURLToken(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/auth/login?token=wrong", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (login page with error)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid token") {
		t.Error("should show error for invalid URL token")
	}
}

func TestManifestAccessibleWithoutAuth(t *testing.T) {
	h := Middleware(testToken, okHandler())
	req := httptest.NewRequest("GET", "/manifest.json", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (manifest must be publicly accessible)", rr.Code)
	}
}

func TestShutdownBlockedOnNetworkListener(t *testing.T) {
	h := Middleware(testToken, okHandler())

	// Even with valid auth, shutdown is forbidden on the network listener.
	req := httptest.NewRequest("POST", "/v1/shutdown", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (shutdown blocked on network listener)", rr.Code)
	}
}

func TestShutdownBlockedWithoutAuth(t *testing.T) {
	h := Middleware(testToken, okHandler())

	req := httptest.NewRequest("POST", "/v1/shutdown", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestTokenWithWhitespaceAccepted(t *testing.T) {
	h := Middleware(testToken, okHandler())

	// Simulate a token pasted with trailing whitespace.
	form := url.Values{"token": {"  " + testToken + "  "}}
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (whitespace should be trimmed)", rr.Code)
	}
}
