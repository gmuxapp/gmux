# ADR 0008: Peer authentication via token

**Status:** Accepted
**Date:** 2026-06-03
**Related:** ADR 0007 (host identity and peer URLs)
**Target:** gmux 2.0 (breaking; no backwards compatibility)

## Context

gmux serves the same HTTP handler on two listeners with two *different*
authentication mechanisms:

| Listener | Bound to | Auth |
|---|---|---|
| TCP / local | `127.0.0.1:8790` | bearer token / cookie (`netauth.Middleware`) |
| Tailnet | tsnet HTTPS `:443` | tailscale identity allow-list (`tsauth.authMiddleware`) |

The tailnet listener wraps the **raw mux** — `tsauth.Start(…, mux)` —
so over the tailnet there is **no token**. The only gate is a `WhoIs`
check of the caller's login name against an allow-list that
**auto-whitelists the node owner** at startup. Peer-to-peer
connections discovered over tailscale carry **no token at all**
(`tsdiscovery` calls `AddPeer` with an empty `cfg.Token`); they rely
entirely on that login allow-list.

Two properties make this dangerous:

1. **The allow-list is all-or-nothing.** A caller who passes the
   `WhoIs` check reaches the *entire* API: `GET /v1/sessions`,
   **`POST /v1/launch`** (spawn an arbitrary command/shell),
   **`/ws/{id}`** (inject keystrokes into any live PTY),
   `ForwardPath` (mutate `projects.json`). There is no read-vs-control
   distinction.

2. **Same-owner identity is the only boundary, and tailnets mix trust
   levels.** Every device on the tailnet that authenticates as the
   owner is fully trusted to execute code on every other gmuxd that
   authenticates as the owner. A tailnet routinely holds nodes of very
   different trust — a laptop, a phone, a VPS, a throwaway container
   running an untrusted AI agent. The documented `docker-tailscale`
   setup joins via *interactive owner login*, so the container
   registers as the owner and lands in the allow-list automatically,
   indistinguishable from the laptop.

### Threat

A container-escape becomes a network-wide remote-code-execution pivot.
An agent (or anything that compromises it) inside a gmux container can
`POST /v1/launch` at the owner's main machine and get a shell, or
attach to a live session and inject input. **The blast radius of the
least-trusted node equals that of the most-trusted node.** This is a
privilege-escalation / lateral-movement flaw, and the auth model is
part of the public contract — so 2.0 is the right (and cheap) place to
fix it.

A mitigating nuance, not a defense: if the container joined as a
**tagged** node (`tag:…`) its `WhoIs` login would not match the owner
login and it would be denied. But the default auto-whitelist and the
docs both steer toward the insecure path, and "safe only if you happen
to tag it" is not a defensible default.

### Scope assumption

**gmux is a personal, single-user, multi-machine tool.** The
documentation is explicit that it is *not* for sharing nodes with other
people. This ADR is decided under that assumption; team / multi-user
sharing is out of scope (see Alternatives B and the deferred path).

## Decision

**Peer connections authenticate by token, everywhere — tailscale
included.** "To talk to a gmux, present its token" becomes the single,
uniform rule, identical to the existing browser login, applied
hub-to-spoke.

### 1. The tailnet listener requires the token

Wrap the tsnet handler with `netauth.Middleware` (token), and keep
`tsauth` as a cheap **outer gate** so only the owner's tailnet identity
can even reach the token prompt:

```
tsnet handler = tsauth.authMiddleware( netauth.Middleware(authToken, mux) )
```

A caller over the tailnet now needs **both** owner identity (`tsauth`)
**and** the host's token (`netauth`). Browsing a host directly over the
tailnet now shows the token login — acceptable because the
token-in-URL / QR flow already makes that paste-once, and viewers
mostly hit the hub, not spokes directly.

### 2. Tailscale autodiscovery is removed

Once every host requires its token, autodiscovery loses its reason to
exist. Its value was *zero-config* — "all my machines just appear" —
but the user must now obtain each host's token to connect, and they get
that token by being on/at that host (`gmuxd auth`). Fetching the URL in
the same breath is free. So discovery's only residual value is *saving
one URL paste*, at the cost of netmap probing, a `/v1/health`
authentication exemption, candidate-surfacing UI, and node_id dedup.
Bad trade.

**`tsdiscovery` is deleted** — the package, its wiring, and the
offline-peers hint in `/v1/health`. Removing it (rather than gating it
off) eliminates the footgun permanently: a config-disabled autoconnect
is an invitation to flip the insecure switch back on.

Tailnet peers are added the same way as any other peer: the
**Connect-to-host** flow (`POST /v1/peers` with `{url, token}`). To
make that one paste, `gmuxd auth` prints a **connect URL** in the
existing token-in-URL format — `https://<fqdn>/auth/login?token=<token>`
— which the Connect modal splits back into `{url, token}`.

**Devcontainer discovery is retained.** It is a separate watcher that
reaches the container locally and reads its token via `docker exec`, so
it is both zero-config *and* tokened — and the pivot threat does not
apply (the host initiates, holding the container's token, never the
reverse).

### 3. One peer-auth path

Every peer — manual, tailnet, or devcontainer — flows through the
**same token-bearing client** (`apiclient.WithBearerToken(cfg.Token)`).
This collapses today's two peer-auth models (manual = token, tailscale
= implicit identity) into one. Less special-casing, not more.

### 4. Secrets flow only toward the controlled node

The direction of a peering connection is **hub → spoke** (the viewer's
machine reads/controls a remote; the remote never connects back). To
control host X you must hold **X's** token. The hub holds each spoke's
token; a spoke never receives the hub's. Compromising a spoke yields
that spoke's own token — which controls the box the attacker already
owns — **never the hub's**. The pivot is closed structurally, by where
the secret lives.

There is no shared-token shortcut: a single token reused across all
nodes would let a compromised spoke control the hub, reopening the
pivot. Each host keeps a distinct token; pairing is per-host.

### 5. Capability seam for the future

The token check is all-or-nothing today (a token grants the full API).
The check is structured to carry a **capability** field later
(e.g. `mirror` vs `control`) so read-only peers become an *addition*,
not a re-plumb. 2.0 ships **control-only** — we slam the door rather
than scope it now.

### 6. The two node identities stay in their layers

This decision deliberately **does not** reconcile gmux's `node_id`
(ADR 0007) with tailscale's `StableNodeID`. Because the **token** is
the authenticator, neither identity needs to authenticate the other:

- gmux `node_id` — durable, app-level: reference anchor, roster key,
  display label. Unchanged from ADR 0007.
- tailscale `StableNodeID` — transport-level: what `WhoIs` proves at the
  `tsauth` gate. Not persisted, not bound to `node_id`.

A node may *optionally* send its `node_id` as a **display-only** header
so a host can label "which device connected" in the UI — but the token
remains the gate; the claimed `node_id` is never trusted for auth. This
adds **no new persistent state** beyond the per-peer token field manual
peers already carry.

## Alternatives considered

### A. Per-`node_id` grant over the tailnet (rejected)

Authorize by gmux `node_id`, authenticated by tsnet `WhoIs`; replace
the owner auto-whitelist with explicit per-node grants. This is the
*more granular* model — per-device revocation, an authenticated "device
X connected" identity, a natural home for capability scoping — and over
the tailnet it needs **no shared secret** because tsnet already
authenticates the node cryptographically.

Rejected for 2.0 because:

- It adds a **new concept** for the user ("go to the peer and run an
  approve command") that exists nowhere else in the product, whereas
  token-pairing reuses the browser-login concept the user already
  knows.
- It adds **persistent state**: a grant store *plus* a
  tailscale-`StableNodeID` ↔ gmux-`node_id` binding (`WhoIs` proves the
  former; `/v1/health` reports the latter).
- A *propose-then-authorize* discovery flow keeps the netmap watcher
  and probing alive — the very machinery we delete by removing
  autodiscovery (§2).
- Its wins — per-device revocation, authenticated per-device identity —
  matter most for **multi-user / team** tailnets, which are explicitly
  out of scope. For a personal fleet, token-everywhere closes the same
  hole with fewer concepts and less state.

### B. Scoped tokens / OAuth (deferred)

If gmux ever supports sharing nodes with other people, the right answer
is **scoped tokens or OAuth** — capability- and identity-scoped
credentials with independent revocation. That is a larger design and a
different threat model (multi-principal), explicitly deferred. The
capability seam (§5) is the forward-compatible hook.

### C. Keep `tsauth` as the sole gate, add capability scoping there
(rejected)

Keeping the login allow-list but splitting read vs control still trusts
*every* owner-authenticated node for *some* access, and still can't
distinguish the laptop from the throwaway container. It narrows the
hole without closing it.

## Consequences

- **One auth model.** "Present the host's token" applies to browsers
  and to peers, local and tailnet alike. The manual-vs-tailscale peer
  split disappears.
- **The RCE pivot is closed** for the personal-tool threat model: a
  compromised spoke cannot reach the hub, because it never holds the
  hub's token.
- **Tailscale autodiscovery is removed**, not merely gated. Tailnet
  peers are added once via Connect-to-host (paste the connect URL from
  `gmuxd auth`). This is inherent to closing the hole — there is no
  shared-token shortcut that preserves zero-config without reopening
  the pivot — and it deletes the `tsdiscovery` package, its netmap
  probing, and the offline-peers hint. **Devcontainer discovery is
  unaffected** (local, tokened via `docker exec`).
- **No new persistent state** beyond the per-peer token already used by
  manual peers. The `node_id` / `StableNodeID` reconciliation is
  avoided entirely.
- **`/v1/health` stays token-gated** (no exemption): the
  Connect-to-host probe carries the token, so there is no
  unauthenticated probe left needing access.
- **Tailnet browser access gains a token login.** Mitigated by the
  existing token-in-URL / QR paste-once flow.
- **`tailscale.allow`** retains meaning only as the `tsauth` outer
  gate (who may reach the token prompt), no longer as the access
  decision itself.

### Known follow-up: tsnet transport for manual tailnet peers

Autodiscovery dialed discovered peers through gmux's embedded tsnet
transport (`peering.WithTransport`). The manual Connect-to-host path
(`POST /v1/peers`) uses the default HTTP transport, which resolves
`.ts.net` MagicDNS names only when the host also runs *system*
tailscaled. A hub whose **only** tailscale connection is gmux's
embedded tsnet therefore can't reach a same-tailnet MagicDNS peer added
manually. This is a pre-existing manual-peer limitation that
autodiscovery masked; it's niche (interactive hubs normally run system
tailscale; containers are spokes, not hubs) and separable from the auth
decision. The fix — routing peers on the local tailnet suffix through a
shared, late-bound tsnet transport — is tracked as a follow-up and is
not part of this change.

## Migration

This is a breaking change appropriate to a major version.

- **Previously auto-discovered peers are migrated into the roster as
  `Auth needed`.** They were never persisted as peers (they lived only
  in the `tsdiscovery` cache, re-added each boot), so on the first
  upgrade start the cache file (`tailscale-discovery.json`) is imported:
  each cached gmux device **that a project references** becomes a
  token-less manual peer, then the cache is deleted. Scoping to
  referenced hosts is deliberate — those are the ones whose references
  would otherwise orphan; other gmux boxes the tailnet once surfaced are
  left out rather than filling the roster with `Auth needed` rows, and
  can be re-added on demand. A migrated host shows as `Auth needed`
  until the user supplies its token via **Add token** (which upserts the
  token onto the existing record, matched by URL). `projects.json` is
  backed up to `projects.json.bak` before any schema migration rewrites
  it.
- **Release notes must call this out loudly:** after upgrading, each
  migrated host needs its token supplied once (`gmuxd auth` on the
  remote, then **Add token** on the hub); hosts that were never
  auto-discovered are added via Connect-to-host as before.
- Docs to update: `multi-machine.md`, `remote-access.md`, and the
  `docker-tailscale` example — "on my tailnet" must no longer imply
  "can drive my machines"; pairing is now an explicit per-host token
  step.
