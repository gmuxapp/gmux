# ADR 0003: Daemon-initiated resume passes Session.ID to the runner

**Status:** Proposed
**Date:** 2026-05-04
**Related:** ADR 0004 (SessionStream and the live↔dead view abstraction)

## Context

Resuming a dead session — re-running its command in a fresh runner
process while keeping its identity in the daemon's store — is
implemented today by a deferred match-and-merge:

1. The daemon's `/v1/resume` handler stashes the resumed session's
   command in a `pendingResumes` map keyed by command, then forks
   a `gmux` runner.
2. The forked runner generates a fresh native session id via
   `naming.SessionID()`, opens its scrollback file at
   `sessions/<runner-native-id>/scrollback`, binds its socket at
   `/tmp/gmux-sessions/<runner-native-id>.sock`, and registers with
   gmuxd carrying that fresh id.
3. `discovery.Register` on the daemon notices the pending-resume
   entry whose command matches the new runner's command, and
   **merges** the new live registration into the existing dead
   record: keeps the dead session's id, updates its socket / pid /
   alive flag from the new registration, removes the new
   registration's stray identity from the store.

This shape produces a recurring set of problems:

1. **Two ids, two on-disk locations.** The runner writes its
   scrollback to `sessions/<runner-native-id>/scrollback`, but
   gmuxd's broker, sessionmeta record, and SSE protocol all key on
   the post-merge id. After resume, the bytes the user actually
   wants live at one path; the broker reads from another.
2. **Match-by-command is fragile.** Two dead sessions with byte-
   identical commands (cwd-aware shell `bash` invocations, scripted
   adapters with no per-instance args) cannot be reliably told
   apart. The current code uses command-equality with no further
   discrimination.
3. **Scrollback path mismatch on the resume seam.** The
   broker-vs-writer path divergence means a cold-load of a
   just-resumed dead session returns "no scrollback was captured"
   even when bytes are intact on disk under the runner-native-id
   directory. (Empirically verified during S5 development.)
4. **Indirection pressure.** The natural fix without changing the
   resume model is to add a `RunnerID` field on `store.Session`
   that points at the latest runner-native-id, and have the broker
   resolve through it. That is a permanent piece of new state, a
   new field in meta.json, and a new read-time lookup, all to
   reconcile a divergence the resume flow itself created.
5. **Merge bookkeeping.** `pendingResumes`, the merge branch in
   `Register`, the duplicate-cleanup `sessions.Remove(newSess.ID)`
   call, the conditional `Subscribe(existingID, ...)` plumbing, and
   the resume-merge persistence callback all exist exclusively to
   reconcile two ids that the daemon itself caused to diverge.

The root cause is that the daemon already knows the existing
Session.ID at resume time but doesn't tell the runner. The runner
generates an id it didn't need to.

## Decision

The daemon's resume handler **passes the existing Session.ID to the
forked runner** via a dedicated `GMUX_RESUME_ID` environment
variable. The runner uses it as its session id instead of
generating a fresh one.

```go
// services/gmuxd/cmd/gmuxd/main.go (resume handler)
cmd := exec.Command(gmuxBinary, "--", session.Command...)
cmd.Env = append(filteredEnv, "GMUX_RESUME_ID="+session.ID)
```

```go
// cli/gmux/cmd/gmux/run.go
sessionID := os.Getenv("GMUX_RESUME_ID")
if sessionID == "" {
    sessionID = naming.SessionID()
}
```

`GMUX_RESUME_ID` is a one-shot directive separate from the
`GMUX_SESSION_ID` the runner already exports to its child process.
The two have orthogonal meanings: `GMUX_SESSION_ID` is the
runner-published "this is my id" (consumed by adapters and hooks);
`GMUX_RESUME_ID` is the daemon-supplied "adopt this id at
startup" (consumed only by the runner's own initialisation).
Using distinct names prevents nested-gmux invocations from
accidentally re-binding the parent session's id: a `gmux foo`
inside an interactive gmux session inherits `GMUX_SESSION_ID` from
the parent runner but does not inherit `GMUX_RESUME_ID` (the
daemon never set it for the parent), so the nested invocation
generates a fresh id as expected.

### Register simplifies

`discovery.Register` becomes a single path with no merge branch:

```go
func Register(...) error {
    newSess, err := queryMeta(socketPath)
    if err != nil { return err }

    // Re-registration of a known id (resume, daemon restart with
    // surviving runner, transient socket-file race): preserve the
    // slug already resolved by the adapter; only the runtime fields
    // change.
    if existing, ok := sessions.Get(newSess.ID); ok {
        newSess.Slug = existing.Slug
    } else if a := adapters.FindByKind(newSess.Kind); a != nil {
        if reg, ok := a.(adapter.SessionRegistrar); ok {
            info, _ := reg.OnRegister(newSess.ID, newSess.Cwd, newSess.Command)
            if info.Slug != "" { newSess.Slug = info.Slug }
        }
    }

    sessions.Upsert(*newSess)
    if !newSess.Alive && newSess.Peer == "" && onDead != nil {
        onDead(*newSess)
    }
    if subs != nil { subs.Subscribe(newSess.ID, socketPath) }
    if fileMon != nil { fileMon.NotifyNewSession(newSess.ID) }
    return nil
}
```

The `pendingResumes` map, its `Add` / `Take` plumbing, the merge
block, the resume-merge persistence callback, and the
duplicate-cleanup logic all delete.

### Disk layout stays one-per-Session.ID

```
~/.local/state/gmux/sessions/<Session.ID>/
    meta.json       # gmuxd writes
    scrollback      # runner writes (truncate-on-Open at startup)
    scrollback.0    # rotated previous file
```

A resumed runner opens the same directory the dead runner used.
The truncate-on-Open semantic of `scrollback.Open` naturally
discards the dead run's bytes from disk; in-memory scrollback that
the user is actively viewing is preserved by the frontend (see
ADR 0004). No `RunnerID` field, no broker-side resolution
indirection, no migration of files at merge time, no orphaned
runner-id directories to reap.

### Slug preservation on re-registration

When an existing id re-registers, `discovery.Register` skips the
adapter's `OnRegister` hook and reuses the already-resolved slug.
Two reasons:

1. **Slug stability across resume.** The dead session's persisted
   record (loaded by `sessionmeta.Sweep` at daemon startup) carries
   the slug as it was at time of death — which may have been
   refined by the file-monitor attribution pipeline well after
   original registration (e.g., a claude session whose slug
   becomes the conversation summary). On resume, this persisted
   slug is the authoritative one. Re-running `OnRegister` would
   either return a stale initial slug (shell: cwd basename) or
   nothing (tool adapters don't implement `OnRegister`), in either
   case potentially clobbering the post-attribution slug. Skipping
   `OnRegister` and preserving the existing record's slug keeps
   `juniper` as `juniper` across the seam.

2. **Idempotence cost.** `Shell.OnRegister` writes a state file
   used for daemon-restart rediscovery, keyed by `(cwd,
   sessionID)`. Under id-passthrough that file already exists
   from the original registration; rewriting it produces an
   identical blob. Skipping the call avoids the redundant fs
   write.

Adapters whose `OnRegister` does non-idempotent work beyond slug
derivation can opt in to running on re-registration via a new
capability flag if a need surfaces; no current adapter is in this
category.

### Collision handling

The runner detects an `EADDRINUSE` on socket bind, distinguishes a
stale socket file from a live owner, and falls back to a fresh
`naming.SessionID()` if and only if a live owner is found:

```go
// cli/gmux/internal/ptyserver/ptyserver.go
func bindSocket(sockPath string) (net.Listener, error) {
    _ = os.Remove(sockPath)                  // tolerate stale file
    ln, err := net.Listen("unix", sockPath)
    if err == nil { return ln, nil }
    if probeSocket(sockPath) {               // live owner
        return nil, errIDInUse
    }
    return nil, err                          // bind failed for other reason
}
```

```go
// cli/gmux/cmd/gmux/run.go
listener, err := bindSocket(sockPath)
if errors.Is(err, errIDInUse) {
    if requestedID != "" {
        log.Printf("gmux: id %s already live; starting as new session", requestedID)
    }
    sessionID = naming.SessionID()
    sockPath = filepath.Join(socketDir, sessionID+".sock")
    listener, err = bindSocket(sockPath)
}
```

When the fallback fires, the runner registers with its fresh id;
the daemon's `Register` handles it as a brand-new session via the
"unknown id" branch; the user sees a new session in the sidebar
alongside the still-live original. The slug-deconfliction path
will rename the new session (e.g., `juniper-2`). The user can
dismiss whichever they don't want.

The `/v1/resume` HTTP response continues to return the
**requested** session id immediately after fork, before the runner
registers. In the rare collision case the response is
optimistically incorrect; the SSE stream surfaces the actual new
session within milliseconds and the frontend reconciles. Blocking
the HTTP response until registration would tighten correctness at
the cost of coupling the resume handler to the registration event
loop, and is not worth the complexity for a pathological case.

### When collision can occur

Three paths produce a live owner of a requested id:

1. **Double-click race.** User clicks Resume twice within the
   fork-to-register window. Mitigation: the resume handler marks
   the session as resuming in the store at fork time and rejects
   subsequent resume requests until alive flips. The runner-side
   fallback covers any race that slips past the daemon-side guard.
2. **Daemon restart with surviving runner, mid-scan resume.**
   Daemon is restarted while a runner is alive. If the session
   was previously persisted by a prior death cycle
   (`sessionmeta.Sweep` finds an entry), it loads as dead;
   discovery's first `Scan` then rediscovers the live socket and
   flips it back to alive via the re-registration branch. If the
   session was continuously alive (never persisted), the store
   is empty for it at startup and `Scan` registers it fresh.
   Either way: a Resume click in the gap between
   sessionmeta-load and the first scan completing sees the
   session as dead and forks a new runner. The new runner's bind
   on `R1.sock` collides with the survivor; the runner-side
   fallback produces a fresh-id sibling session. The original
   `R1` is rediscovered correctly by the next scan; the user
   ends up with their session intact plus an orphan to dismiss.
3. **Defense in depth.** Future protocol work involving
   cross-host migration or peer takeover could create
   harder-to-reason-about windows. The runner-side fallback is a
   protocol-level invariant — "the runner always starts" — that
   does not require all callers to be perfect.

## Consequences

### Positive

- **Single source of truth for session identity on disk.** One
  directory per Session.ID, written by exactly one runner instance
  at a time. The whole class of "the bytes are at one path, the
  broker reads from another" bugs structurally cannot occur.
- **Simpler `Register`.** `pendingResumes` and the merge branch
  delete. The function fits on one screen with one branch
  (re-registration vs. fresh).
- **Simpler resume handler.** No pending-state map to manage, no
  cleanup-on-failure path beyond the existing process-launch error
  handling.
- **No new persistent state.** Avoids the `RunnerID` field that
  would otherwise be the natural indirection. meta.json schema is
  unchanged.
- **Idempotent re-registration.** A runner that registers, drops,
  and re-registers (e.g., across a transient network blip in a
  future remote-runner scenario) is handled by the same code path
  as resume.
- **Truncate-on-Open does the right thing.** The scrollback file
  semantics that already exist for fresh sessions extend naturally
  to resumed sessions — no special-case "preserve the previous
  run's bytes" code on the disk side; whatever preservation the
  frontend wants happens in the byte buffer (ADR 0004).

### Negative

- **Adapter `OnRegister` skipped on re-registration.** Adapters
  whose `OnRegister` does non-idempotent or stateful work beyond
  slug derivation will not run that work on resume. Audit of
  current adapters shows none in this category; future adapters
  needing it will require a capability flag.
- **The `/v1/resume` HTTP response can be optimistically wrong on
  collision.** Documented above; SSE reconciles. Tightening
  requires additional coupling.
- **Daemon-restart-with-surviving-runner mid-scan window.**
  Resume clicked during the gap between sessionmeta-load and the
  first discovery scan can produce a transient orphan session.
  Recoverable via dismiss; not a regression vs. today.

### Breaking

None. The change is internal to gmuxd ↔ runner protocol; the HTTP
and SSE surfaces are unchanged. Existing on-disk session records
continue to load and resume correctly without migration. Cross-
version peers are unaffected (the resume flow is fully local to a
single daemon).

## Alternatives considered

### A. Status quo: match-by-command, merge after registration

Rejected as the source of the issues catalogued in Context. The
match-by-command discrimination is unreliable for shells and
scripted adapters; the path divergence on disk is unfixable
without either renaming directories or carrying a `RunnerID`
indirection. Both options are strictly more state and code than
the chosen decision.

### B. Add `RunnerID` field on `store.Session`; broker resolves through it

Rejected. The natural minimum-touch fix to the path-divergence
bug, but it permanently encodes the divergence in the data model:
a new field on every session, a new column in meta.json, a new
indirection on every scrollback read, plus a merge-time field
update and a Sweep-time orphan-dir reaper. Costs more state than
the chosen decision saves complexity. Decision (passthrough)
makes RunnerID structurally unnecessary because Session.ID and
the live runner's id are always equal.

### C. Migrate scrollback files at merge time by renaming the directory

Rejected. Renaming directories under live writers is correct on
POSIX (open FDs follow the inode) but adds a fs-touching step to
the merge hot path, requires careful handling of partial states
during a daemon crash mid-rename, and still leaves the merge
branch intact in `Register`. Solves the symptom while preserving
the cause.

### D. Daemon blocks the `/v1/resume` HTTP response until the runner
registers, returning the actual id

Rejected for v1. Tightens the optimistic-response correctness
property at the cost of coupling the resume handler to the
discovery event loop with a wait condition. The collision case is
rare and self-correcting via SSE; deferred until a real user
report shows the optimistic response causing observable harm.

### E. Pass Session.ID via a CLI flag (`gmux --session-id=R1`) instead
of an env var

Rejected. Both work; env var is more contained (no addition to the
public CLI surface, not user-invokable in normal use). The env var
name (`GMUX_RESUME_ID`) is dedicated to this directive and
distinct from the `GMUX_SESSION_ID` the runner exports to its
child, so there is no aliasing with adapter / hook env.

### F. Have the runner ask the daemon for an id assignment at startup

Rejected. Inverts the natural "daemon orchestrates, runner runs"
relationship. Adds a daemon round-trip before bytes can flow.
Provides no benefit over the env-var passthrough since the daemon
already knows the id at fork time.

### G. Reuse `GMUX_SESSION_ID` (which the runner already exports
for its child process) as the daemon→runner channel

Rejected. `GMUX_SESSION_ID` is set by the runner for its child;
a nested `gmux foo` invocation inside an interactive gmux session
inherits it from the parent shell. If the runner read
`GMUX_SESSION_ID` on startup as a directive, a nested invocation
would attempt to re-bind the parent session's id, depending on
the collision fallback to recover. That works (the user ends up
with a fresh-id sibling session) but accidentally relies on a
safety net for the common path. A dedicated `GMUX_RESUME_ID`
name is set only by the daemon for resume / restart launches and
is never inherited by descendants, eliminating the false-positive.

### H. Use the slug as the persistent identity and let Session.ID
churn

Rejected. Slug is mutable (deconfliction renames, user-driven
edits in future) and not guaranteed unique across (kind, peer)
boundaries. Session.ID is the right level of stability for the
"this specific session record" identity. Slug remains the
human-readable identity it already is.
