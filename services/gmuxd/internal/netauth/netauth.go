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
	_ "embed"
	"encoding/base64"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/authtoken"
)

// brandFontWOFF2 is Instrument Sans 700, subset to only the wordmark glyphs
// ("gmux"). It is OFL-licensed and embedded so the pre-auth login page can
// render the brand font without any outbound request. The app's full
// @fontsource copy lives behind auth in the web bundle; this standalone
// page cannot reach it, so it carries its own ~1 KB subset.
//
//go:embed brand-instrument-sans-700.woff2
var brandFontWOFF2 []byte

// brandFontFace is an inline @font-face rule whose src is a self-contained
// data: URI built from brandFontWOFF2 (no network fetch).
var brandFontFace = `@font-face{font-family:'Instrument Sans';font-style:normal;font-weight:700;font-display:swap;src:url(data:font/woff2;base64,` +
	base64.StdEncoding.EncodeToString(brandFontWOFF2) + `) format('woff2');}`

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
		// The control plane is a full terminal: never allow it to be
		// embedded in another origin's frame (clickjacking → keystroke
		// injection). Set on every response from the network listener.
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")

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

		switch authMethod(r, token) {
		case authBearer:
			// A bearer header cannot be attached by a foreign origin's
			// page, so bearer-authed requests carry no origin constraint.
			next.ServeHTTP(w, r)
			return
		case authCookie:
			// Cookies ARE attached cross-origin. SameSite=Strict blocks
			// cross-SITE attachment, but ts.net is on the Public Suffix
			// List, so every co-tenant service on the same tailnet is
			// same-site with gmux and SameSite does nothing between them.
			// Enforce same-ORIGIN for anything state-changing: mutating
			// methods and WebSocket upgrades (WS → PTY is remote code
			// execution). Reads over GET stay unconstrained because a
			// cross-origin page cannot read the response anyway (no CORS
			// headers are ever emitted).
			if requiresSameOrigin(r) && !isSameOrigin(r) {
				log.Printf("netauth: blocked cross-origin cookie-authed %s %s (Origin=%q, Host=%q) from %s",
					r.Method, r.URL.Path, r.Header.Get("Origin"), r.Host, r.RemoteAddr)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"cross_origin","message":"cookie-authenticated request must be same-origin; use a bearer token for programmatic access"}}`))
				return
			}
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

// authKind identifies which credential authenticated a request. The
// distinction matters because cookies are ambient (the browser attaches
// them regardless of who initiated the request) while bearer headers are
// explicit and cannot be forged cross-origin.
type authKind int

const (
	authNone authKind = iota
	authBearer
	authCookie
)

func authMethod(r *http.Request, token string) authKind {
	// Check Authorization header.
	if h := r.Header.Get("Authorization"); h != "" {
		val := strings.TrimPrefix(h, "Bearer ")
		if val != h && authtoken.Equal(val, token) {
			return authBearer
		}
	}

	// Check cookie.
	if c, err := r.Cookie(cookieName); err == nil && authtoken.Equal(c.Value, token) {
		return authCookie
	}

	return authNone
}

func isAuthorized(r *http.Request, token string) bool {
	return authMethod(r, token) != authNone
}

// requiresSameOrigin reports whether a cookie-authenticated request must
// prove it originated from the gmux UI itself. WebSocket upgrades (terminal
// I/O = code execution) and mutating methods qualify; plain reads do not.
func requiresSameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// isSameOrigin reports whether the browser that issued the request was on
// the gmux control origin itself. It compares against the request's OWN
// host rather than a configured canonical name, so every legitimate way of
// reaching the daemon (localhost, LAN IP, tailscale FQDN, reverse-proxy
// vhost) works without configuration: whatever host the browser used to
// load the UI is, by definition, the host it sends in both Origin and Host.
func isSameOrigin(r *http.Request) bool {
	// Fetch metadata is the most precise signal when present (all
	// evergreen browsers send it).
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "cross-site", "same-site":
		// Explicitly attested as NOT same-origin. "same-site" is exactly
		// the ts.net co-tenant case this check exists for.
		return false
	}

	// Older browsers: fall back to comparing Origin against the host the
	// request itself was addressed to. Browsers always send Origin on
	// WebSocket handshakes and on non-GET fetch/XHR/form requests.
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if hostsMatch(u.Host, r.Host) {
		return true
	}
	// Behind a reverse proxy that rewrites Host, the browser-facing host
	// arrives in X-Forwarded-Host (Traefik forwards Host verbatim by
	// default, but not every proxy does).
	//
	// Trusting a client-suppliable header here is safe against browser-
	// mediated attacks, which are the only ones this check defends
	// against (a non-browser attacker holding the cookie holds the token
	// itself and needs no CSRF). A hostile page cannot reach this branch
	// with a matching pair: any browser that can attach a custom
	// X-Forwarded-Host via fetch() also sends Sec-Fetch-Site (handled
	// above, returns false for cross-site/same-site), and the custom
	// header makes the request non-simple, forcing a CORS preflight that
	// fails because gmuxd never emits Access-Control-Allow-* headers.
	// Browser WebSocket APIs cannot set custom headers at all.
	if xfh := firstForwarded(r.Header.Get("X-Forwarded-Host")); xfh != "" {
		if hostsMatch(u.Host, xfh) {
			return true
		}
	}
	return false
}

// firstForwarded returns the first hop of a possibly comma-separated
// X-Forwarded-* header value, trimmed.
func firstForwarded(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// hostsMatch compares two host[:port] strings, treating explicit default
// ports (:80, :443) as equivalent to their absence.
func hostsMatch(a, b string) bool {
	return normalizeHost(a) == normalizeHost(b)
}

func normalizeHost(h string) string {
	h = strings.ToLower(h)
	h = strings.TrimSuffix(h, ":80")
	h = strings.TrimSuffix(h, ":443")
	return h
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
				setAuthCookie(w, r, token)
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

		setAuthCookie(w, r, token)
		log.Printf("netauth: successful login from %s", r.RemoteAddr)
		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func setAuthCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is keyed on the transport the login actually used: the
		// tailscale listener and TLS-terminating reverse proxies are
		// https (Secure sticks), while plain-http localhost/LAN access
		// must not set it or the browser drops the cookie entirely.
		Secure: requestIsTLS(r),
	})
}

// requestIsTLS reports whether the browser-facing transport is HTTPS,
// either directly (tsnet listener terminates TLS in-process) or via a
// reverse proxy that terminated TLS upstream.
func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https")
}

func serveLoginPage(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	errorHTML := ""
	bounceScript := ""
	if errMsg != "" {
		errorHTML = `<div class="error" role="alert">` + errMsg + `</div>`
	} else {
		// Self-heal: a cross-site-initiated navigation (e.g. tapping through
		// from a browser error page while the daemon was down) makes
		// the browser withhold the SameSite=Strict cookie, stranding an
		// authenticated user here. We are now on the gmux origin, so
		// location.replace('/') is same-site and DOES send the cookie.
		// The timestamp guard fires once, then self-expires.
		bounceScript = `<script>
(function(){ try {
  var k = 'gmux-auth-bounce';
  var now = Date.now();
  var last = parseInt(sessionStorage.getItem(k) || '0', 10);
  if (now - last > 10000) {
    sessionStorage.setItem(k, String(now));
    location.replace('/');
  }
} catch (e) {} })();
</script>`
	}

	// Inline page with no JavaScript and no auth-gated assets (the app
	// bundle is behind this very gate, so the page must stand alone).
	// Colors mirror the app's tokens (oklch, near-black blue-tinted
	// surfaces and the teal accent) with hex fallbacks for older engines.
	// No external resources are loaded: the page stands fully alone so it
	// works air-gapped/offline and leaks nothing pre-auth. The wordmark
	// uses an embedded Instrument Sans subset, inlined as a data: URI, with
	// a system-font fallback.
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>gmux — sign in</title>
` + bounceScript + `
<style>
  ` + brandFontFace + `
  *, *::before, *::after { box-sizing: border-box; }
  :root {
    --bg: #0c0e11; --surface: #16191e; --border: #2b2f36;
    --border-strong: #353a42; --text: #e2e5e9; --muted: #9aa0a8;
    --accent: #3bc4c9; --accent-hover: #4ad4d9; --error: #e5705f;
  }
  @supports (color: oklch(0% 0 0)) {
    :root {
      --bg: oklch(12% 0.015 250); --surface: oklch(19% 0.015 250);
      --border: oklch(32% 0.015 250); --border-strong: oklch(38% 0.015 250);
      --text: oklch(90% 0.01 250); --muted: oklch(65% 0.01 250);
      --accent: oklch(72% 0.1 195); --accent-hover: oklch(78% 0.11 195);
      --error: oklch(65% 0.18 25);
    }
  }
  body {
    font-family: system-ui, -apple-system, sans-serif;
    background: var(--bg); color: var(--text);
    display: flex; flex-direction: column; align-items: center; justify-content: center;
    min-height: 100vh; margin: 0; padding: 24px;
    -webkit-font-smoothing: antialiased;
  }
  .card {
    background: var(--surface); border: 1px solid var(--border);
    border-radius: 12px; padding: 32px 28px; max-width: 380px; width: 100%;
    box-shadow: 0 12px 40px rgba(0,0,0,0.45);
  }
  .brand {
    font-family: 'Instrument Sans', system-ui, -apple-system, sans-serif;
    font-size: 20px; font-weight: 700; letter-spacing: -0.04em;
    color: var(--text); margin-bottom: 20px;
  }
  h1 { font-size: 15px; font-weight: 600; margin: 0 0 18px; color: var(--text); }
  .error {
    font-size: 13px; line-height: 1.4; color: var(--error);
    background: color-mix(in srgb, var(--error) 12%, transparent);
    border: 1px solid color-mix(in srgb, var(--error) 35%, transparent);
    border-radius: 6px; padding: 9px 11px; margin: 0 0 16px;
  }
  input[type="password"] {
    width: 100%; padding: 11px 12px; font-size: 14px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    background: var(--bg); color: var(--text);
    border: 1px solid var(--border-strong); border-radius: 7px; outline: none;
    transition: border-color 0.12s, box-shadow 0.12s;
  }
  input[type="password"]:focus {
    border-color: var(--accent);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 30%, transparent);
  }
  button {
    width: 100%; padding: 11px; margin-top: 14px; font-size: 14px;
    background: var(--accent); color: var(--bg); border: none; border-radius: 7px;
    cursor: pointer; font-weight: 600; transition: background 0.12s;
  }
  button:hover { background: var(--accent-hover); }
  .hint {
    font-size: 12.5px; line-height: 1.55; color: var(--muted);
    margin: 18px 0 0; padding-top: 16px; border-top: 1px solid var(--border);
  }
  code {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    background: var(--bg); border: 1px solid var(--border);
    padding: 1px 5px; border-radius: 4px; font-size: 12px; color: var(--text);
  }
  .tip {
    font-size: 12px; line-height: 1.5; color: var(--muted);
    text-align: center; max-width: 380px; margin: 16px auto 0;
  }
  .tip a { color: var(--accent); text-decoration: none; white-space: nowrap; }
  .tip a:hover { text-decoration: underline; }
</style>
</head>
<body>
<main class="card">
  <div class="brand">gmux</div>
  <h1>Enter your access token</h1>
  ` + errorHTML + `
  <form method="POST" action="/auth/login" autocomplete="off">
    <input type="password" id="token" name="token" required autofocus
           aria-label="Access token" placeholder="Paste token" autocomplete="off"
           autocapitalize="off" autocorrect="off" spellcheck="false">
    <button type="submit">Sign in</button>
  </form>
  <p class="hint">Run <code>gmuxd auth</code> on the host for instructions.</p>
</main>
<p class="tip">One gmux can show every machine's sessions in a single sidebar. <a href="https://gmux.app/multi-machine/" target="_blank" rel="noopener">Multi-machine setup →</a></p>
</body>
</html>`))
}
