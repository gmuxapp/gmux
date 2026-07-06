# ADR 0020: Same-origin enforcement for cookie-authenticated requests

**Status:** Accepted
**Date:** 2026-07-06
**Related:** ADR 0008 (peer authentication via token); supersedes the
documentation approach attempted in PR #292

## Context

`netauth.Middleware` gates the network listeners (TCP and tsnet) with two
credentials:

- **Bearer token** (`Authorization: Bearer …`) — set explicitly by CLI,
  API clients, and hub↔spoke peering.
- **Session cookie** (`gmux-token`, `HttpOnly SameSite=Strict`) — set by
  the browser login flow and attached *ambiently* by the browser to every
  request targeting the gmux host, regardless of which page initiated it.

PR #292 argued this was sufficient to skip WebSocket `Origin` checks
(`AcceptOptions.InsecureSkipVerify`) and Host validation entirely: the
token blocks DNS rebinding, and `SameSite=Strict` blocks cross-site
cookie attachment. That reasoning holds for **cross-site** attackers but
conflates *site* with *origin*:

`ts.net` (and `*.c.ts.net`) is on the Public Suffix List, so the
registrable domain — the "site" for SameSite purposes — is
`<tailnet>.ts.net`. **Every other web service on the user's own tailnet
is same-site with gmux.** `SameSite=Strict` provides no isolation between
same-site origins, so the cookie *is* attached on requests from
`anything-else.<tailnet>.ts.net` to gmux. A compromised or XSS'd
co-tenant tailnet service can therefore CSRF the mutating `/v1/`
endpoints or hijack the `/ws/{session}` WebSocket — which carries raw PTY
I/O, i.e. remote code execution via `POST /v1/launch` or keystroke
injection. Tailscale's `WhoIs` gate does not help: the request arrives
from the user's own authorized device.

An earlier iteration of #292 tried a **Host-header allowlist**
(`GMUXD_TRUSTED_HOSTS`): validate `Host` against a configured list of
trusted names. It was rejected, and that rejection stands: the daemon is
legitimately reachable by many names at once — `localhost`, LAN IPs, the
tailnet FQDN, any reverse-proxy vhost (the documented Traefik + PocketID
pattern) — so an allowlist must enumerate them all, is brittle, and
misconfigures into a lockout. It also adds nothing against DNS rebinding,
which the token already defeats (a rebound page has neither token nor
cookie).

## Decision

**Cookie-authenticated requests must be same-origin. Bearer-authenticated
requests carry no origin constraint.**

The middleware distinguishes which credential authenticated the request:

1. **Bearer** → pass through. A bearer header cannot be forged
   cross-origin (a hostile page cannot make the victim's browser attach
   it), so CLI, API, and peering traffic is untouched.
2. **Cookie** → for WebSocket upgrades and mutating methods
   (`POST`/`PUT`/`PATCH`/`DELETE`), require proof of same-origin;
   otherwise `403`. Plain `GET` reads stay unconstrained because gmuxd
   never emits CORS headers, so a foreign page cannot read the response;
   WS upgrades are checked despite being `GET` because a hijacked socket
   *is* readable cross-origin.

Same-origin proof, in order:

- `Sec-Fetch-Site: same-origin` → allow; `cross-site`/`same-site` →
  deny (an explicit browser attestation wins over everything else —
  `same-site` is exactly the ts.net co-tenant case).
- Otherwise `Origin` must match the request's **own** `Host` (or the
  first hop of `X-Forwarded-Host` behind a Host-rewriting proxy), with
  default ports normalized.

Comparing against the request's own host — rather than any configured
name — is what makes this a check and not an allowlist: whatever hostname
the browser used to load the UI is, by definition, the hostname it sends
in both `Origin` and `Host`. Every legitimate deployment shape works with
zero configuration.

Enforcement lives in `netauth.Middleware`, i.e. at the node terminating
the browser connection. The hub→spoke hop of proxied WebSockets uses
bearer auth (`apiclient.DialWS`) and is exempt at the spoke, which is
correct: the spoke cannot see the browser's origin, and the hub already
enforced it.

Trusting the client-suppliable `X-Forwarded-Host` fallback is safe
because the check only defends against *browser-mediated* attacks (a
non-browser attacker holding the cookie holds the token itself). A
hostile page cannot reach the fallback with a matching pair: any browser
able to attach that custom header also sends `Sec-Fetch-Site` (handled
first), and the custom header makes the request non-simple, forcing a
CORS preflight that fails.

Accompanying hardening in the same change:

- `Content-Security-Policy: frame-ancestors 'none'` and
  `X-Frame-Options: DENY` on every network-listener response
  (clickjacking → keystroke injection).
- The auth cookie gains `Secure` when the login transport is TLS
  (`r.TLS` or `X-Forwarded-Proto: https`), and stays non-Secure over
  plain-http localhost so local login keeps working.

## Consequences

- A same-site (or any cross-origin) page can no longer drive the
  cookie-authed control plane; the ts.net co-tenant RCE path is closed.
- All bearer-token flows — CLI, integrations, peering — are byte-for-byte
  unaffected.
- `curl`-style clients replaying the cookie as a header lose mutating
  access; they must send the same value as a bearer token instead. This
  is intentional: the cookie is a browser credential.
- Browsers old enough to send neither `Sec-Fetch-Site` nor `Origin` on
  non-GET requests (pre-2011) cannot mutate state. Accepted.
- The library-level WS origin check stays disabled
  (`InsecureSkipVerify: true` at the `websocket.Accept` call sites):
  its blanket `Origin == Host` rule would break bearer clients that send
  no `Origin`. The middleware performs the auth-aware check upstream.
