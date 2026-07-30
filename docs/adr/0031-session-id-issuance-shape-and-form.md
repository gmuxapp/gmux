# ADR 0031: session ID issuance, shape, and form

**Status:** Accepted
**Date:** 2026-07-30
**Related:** ADR 0011 (runner-owned registration), ADR 0016 (session retention), ADR 0026 (authoritative SQLite state), ADR 0028 (CLI output channels)

## Context

Session IDs are client-minted before registration, serve as durable store keys
and filesystem path components, and remain addressable after a runner exits.
The old `sess-` plus eight-hex shape had no durable-space issuance check, used a
truncated display form on some CLI surfaces, and shared the web URL session slot
with slugs. A retained-row collision could therefore reuse an identity, while
URL prefix matching made IDs and slugs structurally ambiguous.

Foreground launch must remain daemon-optional and attach early. Waiting for a
registration verdict before starting the child would make the daemon a launch
dependency. Re-minting after the child starts is also invalid: the ID is already
embedded in runner state, environment, socket, adapter setup, and scrollback.

## Decision

### Issuance and uniqueness

A new runner performs a read-only durable-ID availability preflight beside the
socket bind, before any ID-dependent setup. A live socket collision or an
existing durable row causes another mint and bind attempt. Resume IDs are
exempt because their expected existence is the identity contract.

The check creates no reservation or other state. Foreground launch skips it
when gmuxd is unavailable and preserves opportunistic registration. Detached
launch performs it after the existing daemon health/autostart step, within the
existing startup budget.

Registration repeats the durable existence check under the coordinator's
lifecycle fence. A direct, claimless new-runner registration for an existing ID
returns the typed `session_id_exists` conflict; discovery and claimed
resume/restart/replacement flows remain exempt. This is the authoritative race
backstop. It does not re-mint because the child has already started: detached
launch fails without printing an ID, while foreground logs the refusal and the
child continues unregistered, matching other registration failures.

Uniqueness is per host. Peer-qualified `@peer` addressing namespaces the rare
cross-host duplicate.

### Shape and validation

New IDs are opaque, bare, fixed-width eight-character lowercase base36 strings
(`[0-9a-z]{8}`), generated from `crypto/rand` with unbiased rejection sampling.
Minting re-rolls all-letter results so every ID contains at least one digit.
Boundary validation uses the fixed-width charset allowlist; the digit rule is a
minting/readability property, not provenance validation.

### URL and printed forms

The web session slot has disjoint exact forms:

- `/project/adapter/fix-auth` addresses an exact slug;
- `/project/adapter/~k3v9q2w1` addresses an exact ID.

The `~` sigil is URL-only because shell tilde expansion makes it unsuitable CLI
syntax. URL routing never prefix-matches IDs.

The CLI prints one form everywhere: `<id>` locally and `<id>@<peer>` remotely.
It accepts a full ID, unique ID prefix, or slug, each optionally peer-qualified.
Exact IDs beat exact slugs; remaining exact or prefix ties are ambiguity errors.
No truncating display form exists.

## Consequences

The preflight removes ordinary retained-space collisions without central mint
state, while registration rejects the narrow race between simultaneous equal
mints. Foreground daemon-optionality and early ID printing remain intact. Old
prefixed development-state IDs fail the new validation; the pre-release clean
state policy in ADR 0026 requires no migration.

Dropping the prefix is conditional on this package: registration conflict
protection and the URL sigil replace its collision and namespace roles. If the
package is split and either protection is removed, the session prefix must
return.

## Rejected alternatives

- **Reservation API:** adds ownership state, expiry, and daemon dependence.
- **Gate child start on registration:** breaks daemon-optional foreground
  launch and delays attach on daemon failure.
- **Re-mint after registration conflict:** cannot atomically rewrite the
  already-running child's identity and paths.
- **Filesystem claims:** retained rows can outlive scrollback directories.
- **Longer or structured IDs:** advisory issuance plus authoritative rejection
  removes the lifetime birthday-risk rationale; time, host, and adapter facts
  belong in store fields rather than the opaque key.
