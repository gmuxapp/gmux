# ADR 0018: Stateful notifications with resolve-once interactive actions

**Status:** Proposed
**Date:** 2026-06-27
**Related:** ADR 0002 (session origin), ADR 0011 (authoritative attribution via agent-hook), ADR 0008 (peer authentication via token)

## Context & Problem

gmux needs to tell the user things out-of-band, and the existing notification path is lacklustre. Two use cases drive a real design:

1. **"Waiting for you":** an agent turn finishes and needs the user's input. A fire-and-forget alert that should reach the user wherever they are and land in a reviewable history.
2. **Approvals:** a consumer (e.g. hako's tool-call gate — see hako ADR-0016) wants the user to **approve or decline** an action, and to block on the answer.

These look different but want the **same primitive**. The approval case merely forces requirements the "waiting for you" case also benefits from:

* Notifications carry optional **actions** (buttons).
* Notifications have **lifecycle state** (pending → resolved) and a **result**.
* Actions are **resolve-once and cross-surface**: tapping *Approve* in Telegram must remove the button in the in-app history and stamp the outcome there too. The user must never see a live button for an already-answered question.
* Resolution can be **programmatic, not only user-driven**: the registrant must be able to resolve a notification itself (its own timeout/cancel), otherwise buttons linger after the underlying request has already been decided elsewhere.
* Delivery is **presence-aware**: surface in-app when a viewer is attached; mirror to an external channel (silent if already delivered live, alerting if the user is away).
* A **history view**, filterable by the originating session.

**A web-page approval surface was rejected.** `ts.net` is on the Public Suffix List, so every node on the user's own tailnet is *same-site* with gmux; a separate forwarding origin buys almost no isolation (`SameSite=Strict` does not separate same-site co-tenants), and it bolts a new web surface onto an RCE-equivalent control plane (`/new`). So **the notification and its inline actions are the surface** — there is no forwarded web UI.

## Decision

Introduce a first-class **Notification** with interactive, resolve-once actions, presence-routed delivery, programmatic resolution, and a history view.

### Wire shape

```
Notification {
  id              string   // gmux-issued, stable across every surface
  kind            "info" | "action"
  title, body     string
  actions         []Action // empty for "info" ("waiting for you")
  origin_session  string?  // provenance hint; display only (see below)
  state           "pending" | "resolved"
  result          ""|"<action.id>"|"expired"|"withdrawn" // set on resolve
  resolved_by     "user"|"caller"|null
  created_at, resolved_at
}
Action { id string; label string; style "default"|"primary"|"danger" }
```

### Lifecycle & mechanics

* **Register.** A local client posts a notification (optional `actions`). gmux returns the `id`.
* **Presence-routed delivery.** Attached viewer → surface in-app. Mirror to a configured external channel — **silent** when already delivered live, **alerting** when the user is away. (Presence in v1 is coarse: attached / not. Richer routing is deferred.)
* **Resolve-once, cross-surface.** First resolution wins, from any surface (in-app tap, Telegram callback) **or** programmatically by the caller. gmux fans the resolved state to every surface it touched: buttons removed, result label stamped (`Approved` / `Expired` / `Withdrawn`). Concurrent actions are idempotent — losers observe "already resolved".
* **Programmatic resolution.** The registrant may `resolve(id, result)`. This is what keeps the in-app button from lingering after the caller's own gate resolves server-side (timeout, cancel, liveness loss). Resolution is therefore **not purely user-driven**.
* **Result vocabulary** (shared with consumers): an **action id** (user chose), `expired` (the caller's optional max-wait elapsed), `withdrawn` (the caller went away or cancelled — see liveness).
* **History.** Resolved notifications persist in a history view, filterable by `origin_session`.

### Await + liveness (the consumer connection)

For notifications whose outcome a consumer **blocks on** (approvals), the register call is a **long-lived request**: the consumer holds it open and receives the resolution as the response/stream.

* **Liveness is a precondition of validity.** If the consumer disconnects — agent died, container restart, host reboot — gmux resolves the notification `withdrawn` and stops soliciting the user. Rationale: *never ask a human to approve an operation whose requester is no longer there to receive the result.*
* **Blip tolerance.** A consumer may reconnect with the same `id` within a short grace window to re-attach to a still-pending notification. Only after the window elapses without reconnect does gmux withdraw. This makes "call/response" simply *reconnection*, not a separate protocol.
* **Info notifications** (no actions) need no await — fire-and-forget into history.

### Auth

The register/resolve API is gated by the **same auth as `/new`** (the control plane). No separate token. The API is **no more powerful than `/new`**: anything that can register a (spoofed) approval prompt could already run arbitrary commands, so sharing the gate introduces **no new boundary**. (Cookie-authed browser requests remain subject to the same-origin hardening tracked separately; local/bearer clients as today.)

**Answer authority** = whoever gmux already authenticates (the tsnet identity allow-list). **No per-notification ACL in v1.**

### Session provenance

Sessions already carry a gmux session id in their environment (ADR 0011 lineage). Consumers — e.g. hako's MCP CLI adapters — **attach this id** to the register call; gmux stores it as `origin_session` and offers **per-session notification history**.

The id is a **display/provenance hint, not a trust signal**: a session can read and rewrite its own env, and since auth is already `/new`-level there is nothing to gain by spoofing it. It improves legibility; it gates nothing.

### Delivery integrations

* **v1:** gmux **shells out** to a user-configured command for external delivery (channel-agnostic).
* **Later:** built-in **preset providers** (Telegram, etc.) configured with IDs/tokens. Inline-action callbacks (e.g. Telegram buttons → resolve) are handled **gmux-internally**, so consumers stay channel-agnostic — they only ever observe "resolved with action X".

## Consequences

### Positive
* One system serves both **"waiting for you"** (actionless) and **approvals** (actions); the actionless case justifies it independent of approvals.
* **No web approval surface** ⇒ no new same-site/CSRF exposure on the RCE control plane.
* **Liveness-bound** approvals can't fire into the void: reboot or agent death auto-withdraws.
* Consumers stay **channel-agnostic**; Telegram/inline complexity stays inside gmux.
* Per-session provenance improves legibility of the history view.

### Negative
* gmux holds **transient in-flight state** + one open await-connection per pending approval (bounded by pending count).
* The programmatic-vs-user resolution race must be carefully **idempotent** (first-wins).
* Presence in v1 is **coarse** (attached / not); richer routing deferred.
* External delivery is **shell-out only** at first.

## Alternatives Considered
* **Forwarded web approval page (gmux port-forwarding):** rejected — `ts.net` is on the PSL ⇒ same-site co-tenancy ⇒ negligible origin isolation, and it adds a web surface to the RCE control plane.
* **Approval queue/gate inside gmux:** rejected — fail-closed/blocking semantics are the *caller's* control flow; gmux would have to model consumer control flow. gmux owns **interaction-state**; the **gate** stays with the caller (hako ADR-0016).
* **A separate auth token for the notification API:** rejected — no new boundary over `/new`.
* **Wall-clock TTL as the primary gate:** rejected in favour of **liveness** as the primary gate; a max-wait TTL is optional caller policy, off by default.
