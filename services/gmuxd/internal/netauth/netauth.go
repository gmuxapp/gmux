// Package netauth provides HTTP middleware and a login endpoint for the
// network listener. It authenticates requests via bearer token (header)
// or session cookie, and serves a minimal login page for browser-based access.
//
// The login flow:
//  1. Browser opens any page without a valid cookie.
//  2. Middleware redirects to /auth/login (the login page).
//  3. User pastes the token and submits.
//  4. POST /auth/login validates the token, sets an HttpOnly cookie, and
//     redirects to /.
//  5. All subsequent requests carry the cookie.
//
// Programmatic clients use the Authorization: Bearer <token> header instead.
package netauth

import (
	"log"
	"net/http"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/authtoken"
)

const (
	cookieName = "gmux-token"
	// cookieMaxAge is 90 days. The token itself doesn't expire, so the cookie
	// just needs to be long-lived enough that users don't have to re-enter it
	// constantly, but short enough that a stolen cookie eventually stops working
	// if the token is rotated.
	cookieMaxAge = 90 * 24 * 60 * 60
)

// Middleware returns an http.Handler that wraps next with token authentication.
// Requests with a valid bearer token or cookie are passed through.
// API/WebSocket requests without valid auth get 401.
// Browser requests without valid auth are redirected to the login page.
func Middleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The login page and its POST handler must be accessible without auth.
		if r.URL.Path == "/auth/login" {
			handleLogin(token, w, r)
			return
		}

		// The web app manifest must be publicly accessible. Browsers fetch
		// it without cookies, so auth-gating it returns the login HTML
		// page which Chrome then fails to parse as JSON.
		if r.URL.Path == "/manifest.json" {
			next.ServeHTTP(w, r)
			return
		}

		// Shutdown is a local-only operation (available via Unix socket).
		// Block it entirely on the TCP listener regardless of auth.
		if r.URL.Path == "/v1/shutdown" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		if isAuthorized(r, token) {
			next.ServeHTTP(w, r)
			return
		}

		// Distinguish API requests from browser navigation.
		if isAPIRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"unauthorized","message":"valid bearer token or session cookie required"}}`))
			return
		}

		// Browser: redirect to login page.
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})
}

func isAuthorized(r *http.Request, token string) bool {
	// Check Authorization header.
	if h := r.Header.Get("Authorization"); h != "" {
		val := strings.TrimPrefix(h, "Bearer ")
		if val != h && authtoken.Equal(val, token) {
			return true
		}
	}

	// Check cookie.
	if c, err := r.Cookie(cookieName); err == nil && authtoken.Equal(c.Value, token) {
		return true
	}

	return false
}

func isAPIRequest(r *http.Request) bool {
	// WebSocket upgrades, API paths, and SSE requests are programmatic.
	if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/ws/") {
		return true
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") || strings.Contains(accept, "text/event-stream") {
		return true
	}
	return false
}

func handleLogin(token string, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Check if already authenticated; redirect to home.
		if isAuthorized(r, token) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		// If the token is in the query string (QR code flow), validate
		// and set the cookie immediately. This avoids displaying the
		// login page when scanning a QR code.
		if qToken := strings.TrimSpace(r.URL.Query().Get("token")); qToken != "" {
			if authtoken.Equal(qToken, token) {
				setAuthCookie(w, token)
				log.Printf("netauth: successful login via URL token from %s", r.RemoteAddr)
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			// Invalid token in URL: show login page with error.
			serveLoginPage(w, "Invalid token in URL.")
			return
		}

		serveLoginPage(w, "")

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			serveLoginPage(w, "Invalid request.")
			return
		}

		submitted := strings.TrimSpace(r.FormValue("token"))
		if !authtoken.Equal(submitted, token) {
			log.Printf("netauth: login attempt with invalid token from %s", r.RemoteAddr)
			serveLoginPage(w, "Invalid token. Check the value and try again.")
			return
		}

		setAuthCookie(w, token)
		log.Printf("netauth: successful login from %s", r.RemoteAddr)
		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func setAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func serveLoginPage(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	errorHTML := ""
	if errMsg != "" {
		errorHTML = `<p style="color:#e74c3c;margin-bottom:1em">` + errMsg + `</p>`
	}

	// Minimal inline page. No external dependencies, no JavaScript required.
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>gmux - Authentication Required</title>
<style>
  *, *::before, *::after { box-sizing: border-box; }
  body {
    font-family: system-ui, -apple-system, sans-serif;
    background: #0a0a0a; color: #e0e0e0;
    display: flex; align-items: center; justify-content: center;
    min-height: 100vh; margin: 0; padding: 1em;
  }
  .card {
    background: #1a1a1a; border: 1px solid #333; border-radius: 8px;
    padding: 2em; max-width: 420px; width: 100%;
  }
  h1 { font-size: 1.2em; margin: 0 0 0.5em; color: #fff; }
  p { font-size: 0.9em; line-height: 1.5; margin: 0 0 1.5em; color: #999; }
  label { display: block; font-size: 0.85em; margin-bottom: 0.4em; color: #ccc; }
  input[type="password"] {
    width: 100%; padding: 0.6em 0.8em; font-size: 0.95em;
    font-family: monospace; background: #111; color: #fff;
    border: 1px solid #444; border-radius: 4px; outline: none;
  }
  input[type="password"]:focus { border-color: #666; }
  button {
    width: 100%; padding: 0.7em; margin-top: 1em; font-size: 0.95em;
    background: #fff; color: #000; border: none; border-radius: 4px;
    cursor: pointer; font-weight: 500;
  }
  button:hover { background: #ddd; }
  .hint { font-size: 0.8em; color: #666; margin-top: 1em; }
  code { background: #222; padding: 0.15em 0.4em; border-radius: 3px; font-size: 0.9em; }
</style>
</head>
<body>
<div class="card">
  <h1>gmux</h1>
  <p>This gmux instance requires authentication. Enter the access token to continue.</p>
  ` + errorHTML + `
  <form method="POST" action="/auth/login" autocomplete="off">
    <label for="token">Access Token</label>
    <input type="password" id="token" name="token" required autofocus
           placeholder="Paste token here" autocomplete="off">
    <button type="submit">Sign In</button>
  </form>
  <p class="hint">Find your token by running <code>gmuxd auth</code> on the host machine.</p>
</div>
</body>
</html>`))
}
