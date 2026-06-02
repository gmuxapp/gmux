# ADR 0007: Host identity, naming, and peer URLs

**Status:** Proposed
**Date:** 2026-05-29
**Related:** ADR 0002 (project ownership from session origin), ADR 0005 (references and the local-peer exception)
**Target:** gmux 2.0 (breaking; no backwards compatibility)

## Context

A host has no single identity in gmux today — it has **four**, and
they disagree:

| Source | Where it comes from | Used for |
|---|---|---|
| `os.Hostname()` | OS/kernel (the **container ID** under Docker) | `/v1/health` `hostname` field; peering self-echo name; devcontainer name fallback |
| `tailscale.hostname` (config, default `gmux`) | `host.toml` | tsnet machine name → FQDN; tailnet discovery *filter* (prefix `gmux-*`) |
| `deriveName()` | devcontainer label → container name → container ID | devcontainer peer name |
| `PeerConfig.Name` | `host.toml` | manual peer name; URL `@<segment>` |

The non-obvious part: a **tailnet-discovered peer is named by the
remote's `/v1/health` `hostname` field — i.e. its `os.Hostname()`**,
not its tailscale hostname. Discovery *filters* on the tailscale
hostname prefix but *names* the peer by its OS hostname. So
`tailscale.hostname` and the name shown in the UI / URL are already
two different things.

This produced the bug that motivated this ADR: a gmuxd running inside
a container, discovered over **tailscale**, appeared as
`@ca75413aec31` — the container ID, because that is what
`os.Hostname()` returns in a container and that is what the health
field reports. Recreating the container (rebuild / `compose up`
recreate / `rm`+`run`) mints a new container ID, so the name — and
every URL that embedded it — changed.

Three ways a peer can be connected today, each naming it differently:

- **Tailscale autodiscovery** (same tailnet) → remote's `os.Hostname()`.
- **Devcontainer autodiscovery** (local Docker) → `deriveName()`.
- **Manual `[[peers]]`** in `host.toml` → user-chosen `Name`.

Two hard constraints in the current design:

- **`host.toml` is never written by gmuxd** (documented invariant;
  the TOML library also loses comments/ordering on round-trip).
- URL identity is `@<name>`, where `<name>` is whichever of the four
  sources applies — so URL stability is hostage to the least stable
  source.

### Goals

1. **One identity per node**, independent of how it is reached.
2. **URL stability** — a host's URL changes only when the host's
   identity genuinely changes, never because of *how* a viewer
   reached it.
3. **Collision safety** — names are unique within a tailnet but can
   collide across tailnets or for non-tailscale hosts.
4. **Human-readable**, short URLs.

## Decision

### 1. One identity function

A single `identity()` value feeds the `/v1/health` `hostname` field,
the peering self-name, the `localHostLabel`, and the URL key:

```
identity() = live tailscale name (from tsnet status)   if tailscale enabled
           = os.Hostname()                              otherwise
```

There is **no generated name and no gmux-side persistence of the
name.** The previously-considered "generate `gmux-adjective-noun` and
write it back to config" approach is dropped (see Alternatives A): the
two cases are already covered — tailscale owns the name when present,
and `os.Hostname()` is stable and meaningful on a real machine when it
is not.

### 2. Tailscale is the authority *and* the state

When tailscale is enabled, the identity is **whatever tsnet settled on
after the coordination server's uniqueness dedup** (`gmux-foo` →
`gmux-foo-1`), read live from the listener — not the requested name.
tsnet persists the node key in its state dir, so the name is sticky
across daemon restarts *and* container recreation **iff that state dir
is persisted** (see §4). There is nothing for gmux to reconcile or
write: we always read the current name.

Consequences for config:

- **Remove `tailscale.hostname`.** The *requested* name is set once
  during onboarding (`gmuxd remote`), defaulting to `os.Hostname()`.
- If a user enables tailscale **outside** onboarding (e.g. hand-edits
  `enabled = true`), the requested name defaults to `os.Hostname()` —
  we never had the chance to ask, and tailscale will dedup it if
  needed.

### 3. Devcontainers: discovery gated on the folder label

Devcontainer autodiscovery **requires the `devcontainer.local_folder`
label**; the name is its basename. No label → not discovered. This
removes the container-ID fallback in `deriveName()` by construction:
a container that would have fallen through to the ID is simply not a
discovered devcontainer.

Note this does **not** fix the motivating bug directly — that was a
*tailscale*-discovered container, fixed by §2 (it now reports its
tailscale name, not `os.Hostname()`). §3 closes the separate
devcontainer-ID-churn path.

### 4. A stable opaque `node_id` for sameness

A 32-byte random `node-id`, generated once and persisted alongside
`auth-token` in the state dir (same `LoadOrCreate` pattern,
`0600`), returned in `/v1/health`. It is **never shown and never
appears in a URL.** Its sole purpose is to answer *"is this the same
node?"* independent of name, address, or connection method.

Crucially this adds **no new persistence requirement**: a peer's
`auth-token` and its tailscale node key already must survive on disk
for any connection to it to be stable. A container that doesn't
persist its state dir already regenerates its auth token (and so is
unreachable by existing viewers) — its `node_id` churning with it is
moot. Co-locating `node_id` with `auth-token` makes "persist this
directory" the single, already-necessary rule.

### 5. Peers as state, not config (2.0)

Remove `[[peers]]` from `host.toml`. The set of peers becomes
**state** in `peers.json` (state dir), managed by a **"Connect to
host"** flow in the Settings UI. The user does **not** assign a name —
on connect we fetch the peer's `/v1/health` and adopt its
self-reported identity (§1). The address (URL / FQDN) is stored
separately as the connection *target*, distinct from the *name*.

gmux thus begins owning a writable state file for the peer list. This
is distinct from the `host.toml` "never written" invariant, which
stands for the user-authored config file.

### 6. URL schema: short name, uniform

The URL is `/@<name>/...`, the peer's **short self-name**, for every
connection method. No method prefix (`@ts-` / `@dev-` / `@peer-`),
no FQDN in the URL. The FQDN is *address metadata*, not identity.

Within a single tailnet, tailscale guarantees unique hostnames, so the
common case has zero collisions and fully stable, portable URLs.

### 7. Add-peer flow: dedup first, then collide

On "Connect to host", fetch `/v1/health` → `(node_id, name)`:

1. **Known `node_id`** → already connected. Optionally record the new
   address as an alternate route. *Not* a collision; never rename.
2. **New `node_id`, name free** → use the name.
3. **New `node_id`, name taken** → genuine collision (two distinct
   hosts, same name). Resolve **viewer-side**: suffix it (`name-2`),
   persist the alias in `peers.json`, and surface a flag. The user may
   optionally give the underlying host a clean global name by renaming
   its tailscale machine or its OS hostname — gmux builds **no remote
   name-state** of its own.

This directly resolves the "same host added via tailscale *and*
manually" case: both report the same `node_id`, so step 1 dedupes
them rather than treating the shared name as a conflict.

### 8. No backwards compatibility

This is a 2.0 break. An unknown or unresolvable `@<name>` redirects to
home (the user locates the session and re-bookmarks). The removed
config keys (`tailscale.hostname`, `[[peers]]`) are rejected at load
with an error pointing to `gmuxd remote` / "Connect to host", rather
than silently migrated.

## Consequences

### Positive

- **One identity** across health, peering, UI labels, and URLs —
  derived from a single function with one obvious source per case.
- **The motivating bug is fixed by construction**: a tailscale host
  reports its (sticky, deduped) tailscale name, not `os.Hostname()`;
  devcontainers can't fall through to a container ID.
- **No name generation, no name writeback, no `host.toml` writes.**
  tailscale owns the name where present; `os.Hostname()` covers the
  rest.
- **Stable, portable, readable URLs** in the common single-tailnet
  case. Method changes (autodiscovered ↔ manual) never churn a URL.
- **Robust sameness** via `node_id`, at no new persistence cost.

### Negative

- **`node_id` is new remote state** (one file). Mitigated by
  co-locating with `auth-token`, which already must persist.
- **Cross-tailnet / non-tailscale collisions still need a human
  decision** (viewer-side suffix + flag, or rename the host). We do
  not auto-globally-resolve these.
- **Identity churns if a container's state dir is not persisted** —
  but such a container is already unreachable (auth token churns too),
  so this is not a regression.
- **Viewer-side suffixes are viewer-relative** (a suffixed `name-2`
  may differ from how another viewer names the same host). Accepted:
  manual peers are inherently viewer-owned config.

### Breaking

- `tailscale.hostname` and `[[peers]]` are removed from `host.toml`.
- Existing `@<name>` URLs that embedded an `os.Hostname()`/container-ID
  name (or a manual `Name`) will not resolve and fall back to home.
- The `/v1/health` `hostname` field changes meaning (now the unified
  identity) and gains `node_id`.

## Alternatives considered

### A. Generate + persist `gmux-adjective-noun`, reconcile with tailscale

The earlier proposal: generate a human-readable name on first start,
persist it, and on tailscale connect compare against the tailscale
name and write back the deduped value. Rejected: tailscale is
*already* the authority and the durable state for the name, so
generation + writeback duplicates that machinery and re-adds a
config/state-write surface. `os.Hostname()` covers the no-tailscale
case without generation.

### B. Method-prefixed URLs (`@ts-` / `@dev-` / `@peer-`)

Rejected. The connection *method is not a stable property of a host*:
the same host moved from autodiscovered to manual would change URL
form, violating Goal 2. It is also viewer-relative (non-portable), and
it does not even close *within-method* collisions (two manual peers,
or two foreign tailnets, can still collide within `@peer-`).

### C. FQDN as the URL key

Rejected. Non-tailscale peers have no FQDN, so it cannot be the
universal key — short-name handling is needed regardless. Using FQDN
only for manual peers reintroduces the same method dependence as (B),
and it is verbose in every URL. The FQDN's correct role is the
connection *address*, stored in `peers.json`.

### D. Remote-settable persisted name without tailscale

The other collision-resolution option: give every node the ability to
persist a user-set name even without tailscale, and tell the user to
change it to resolve a conflict. Rejected: for a host the user
controls, this collapses to "rename the host's tailscale machine name
or OS hostname," which needs no new gmux machinery; building remote
name-state re-adds exactly the persistence/generation surface §1–§2
removed. The viewer-side suffix (§7) covers hosts the user cannot
touch.

### E. Keep `[[peers]]` in `host.toml`

Rejected for 2.0. File-managed peers cannot cleanly carry viewer-side
rename aliases (§7) or live connection state, and the "Connect to
host" UI needs writable state anyway. A user-authored config file is
the wrong home for runtime-mutable peer state.

### F. Keep naming peers by `os.Hostname()` even under tailscale (status quo)

Rejected — it is the bug. In a container `os.Hostname()` is the
container ID; on a tailnet it diverges from the name the host is
actually reachable as.
