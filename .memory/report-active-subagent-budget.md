# Active-subagent budget implementation report

## Scope and product semantics

This slice adds a host-local default budget for **live semantic-agent descendants per current behavioral/presentation root**. It applies to `gmux agent prompt --new` before runner creation. It deliberately does not add depth limits, historical launch budgets, prompt headers, UI/per-root overrides, peer-distributed quota, `--force`, or a launch queue.

- Root ownership comes only from mutable `parent_session_id` plus `promoted_to_root`; `launched_from_session_id` remains forensic provenance and is never consulted.
- Reparenting moves the live semantic-agent subtree immediately. Promotion makes the promoted session a root; the promoted session itself is excluded while its semantic-agent descendants use its budget. Demotion rejoins the containing root.
- Active means an installed, non-fenced local runner generation (live/resident), not an active model turn. Dead/finished retained rows, process/shell children, the root itself, and remote projections are excluded.
- A slot is reserved before the detached runner process, PTY, session row, unread state, or event exists. Registration atomically converts the receipt to installed liveness. Pre-start or registration failure releases the receipt; normal exit/kill releases it after the exit commit when the generation leaves the registry.
- A launch without `GMUX_SESSION_ID` is a new root and consumes no descendant slot. An absent parent remains an orphan/new root; gmux does not infer ownership from cwd or the latest session.

## Configuration contract and precedence

`host.toml` has a top-level integer:

```toml
max_active_subagents = 8
```

The built-in default is 8. A value in `host.toml` overrides it when gmuxd starts. There is no environment, CLI, UI, or per-root override in this slice. Valid values are 1 through 1024; explicit zero, negatives, larger values, malformed TOML, and wrong types fail startup. The website host.toml reference documents precedence, exact counting, lifecycle, and local-daemon scope.

## Mechanism trace

1. `cmdAgentPromptNew` validates prompt text and launch argv first, then POSTs the actual caller parent (`GMUX_SESSION_ID`, or null) to `/v1/agent-launch-reservations`.
2. `Coordinator.ReserveActiveSubagent` holds the lifecycle mutex shared by registration, reparent, promotion, dismissal, and budget liveness edges. Its incremental index resolves the current root and checks installed semantic descendants plus outstanding receipts.
3. A refusal is HTTP 429 with stable code `subagent_limit_reached` and exits the CLI with 1 before spawning. Example:

   ```text
   gmux: subagent_limit_reached: subagent limit reached for root 1abc234d: 8 of 8 active subagents; run 'gmux ls' to inspect this host's sessions
   ```

4. An allowed response returns an opaque 128-bit receipt. The parent passes it only to the detached runner through `_GMXINTERNAL_ACTIVE_SUBAGENT_RESERVATION`.
5. `runSession` captures and unsets that private variable before constructing the agent child's environment, then includes the receipt in `POST /v1/register`.
6. `Coordinator.Register` claims the receipt before runner I/O, verifies the registered row has the exact admitted direct parent, and under its commit/install mutex consumes the receipt while installing the durable row and live index entry. All error returns release a claimed receipt. The CLI also idempotently DELETEs an unclaimed receipt after any launch return. Unclaimed receipts expire after two minutes as crash recovery (the detached startup budget is 30 seconds).
7. Registry removal on an exit event or stream drop takes the same lifecycle mutex and removes the live budget entry. Replacement generations update one existing node; fast-dead replacement releases it.
8. The durable startup snapshot reconstructs parent/promotion/adapter nodes before convergence. Surviving runner registrations repopulate liveness; HTTP routes are installed only after convergence/sweep completes.

The index stores local nodes, child adjacency, cached roots, active counts by root, and short-lived launch receipts. Normal admission is O(1) root lookup plus at most the bounded outstanding receipt set. Ownership changes recompute only the affected subtree. A newly appearing formerly-missing parent also adopts its indexed orphan subtree. Orphans stop at the last present node. Corrupt cycles terminate deterministically at the lexicographically smallest cycle member.

## Race schedules and invariants

- **7/8 simultaneous admission:** 32 goroutines cross one barrier and contend on `Coordinator.mu`; exactly one gets a receipt, 31 get `ErrSubagentLimitReached`. Converting the receipt under the same mutex yields exactly 8 live descendants and zero receipt residue.
- **8/8 simultaneous admission:** 16 barrier-released goroutines all fail; the receipt map remains empty.
- **Admission vs reparent/promote/demote:** all serialize on `Coordinator.mu`. Receipts retain the admitted direct parent ID, so resolving their root after an ownership mutation moves them with the caller. Registration resolves the new session against the latest graph.
- **Admission vs termination:** exit facts commit first; registry removal and active-count decrement happen together under `Coordinator.mu`. Admission sees either occupied-before-removal or free-after-removal.
- **Registration vs dismissal:** dismissal treats a receipt whose direct parent is in the selected subtree as `ErrSubtreeBusy`; it cannot hide the parent between admission and child registration.
- **Generation replacement:** a replacement preserves one live node; fast-dead replacement marks it non-live. Stale-generation removal cannot release a newer generation because registry generation matching gates the budget update.
- **Daemon restart:** durable ownership is loaded first, liveness is reconstructed only from surviving local registrations, and stale exit-less rows are swept before launch routes are served.

## Before/after CLI examples

Below budget, output is unchanged:

```console
$ gmux agent prompt --new --no-wait 'inspect auth'
1def567h
```

At the root limit, no ID is printed because no process/session was created:

```console
$ gmux agent prompt --new --no-wait 'inspect auth'
gmux: subagent_limit_reached: subagent limit reached for root 1abc234d: 8 of 8 active subagents; run 'gmux ls' to inspect this host's sessions
$ echo $?
1
```

Independent top-level calls (no behavioral parent) create independent roots and are not rejected by another root's count.

## Measurement

`BenchmarkActiveSubagentAdmission1000Sessions` builds a 1,000-node chain and repeatedly reserves/releases against its deepest parent. Three local runs on linux/amd64 (Intel i7-9700K) measured:

- 259.3 ns/op, 32 B/op, 1 alloc/op
- 268.6 ns/op, 32 B/op, 1 alloc/op
- 289.3 ns/op, 32 B/op, 1 alloc/op

This demonstrates admission does not scan/rebuild the 1,000-session graph. The allocation is the cryptographic receipt token.

## Test and build evidence

Focused:

- `go test -race ./services/gmuxd/internal/sessioncoord -run ActiveSubagent -count=3` — pass
- deterministic root/count table: nested, promoted, reparented, dead, process, remote absence, orphan, cycle
- registration conversion, normal termination release, registration failure release, restart reconstruction
- real daemon reservation handler allowed/rejected/release/top-level/validation contracts
- real CLI reservation HTTP path, structured refusal/exit 1, no spawn on rejection, receipt propagation/release
- `go test ./services/gmuxd/internal/sessioncoord -run '^$' -bench ActiveSubagentAdmission1000 -benchmem -count=3` — results above

Repository gates run locally:

- `pnpm lint` — pass
- `pnpm build` — pass
- full Go tests for `cli/gmux`, `services/gmuxd`, and all packages — pass except the unrelated environment-sensitive `TestPiSubcommandsMatchHelp`, whose installed `pi --help` changed its Commands block; all adapter tests pass with that single probe skipped
- `pnpm exec moon run gmuxd:check-centralstore-generated` — pass
- `pnpm exec moon run gmuxd:check-centralstore-cross-build` — pass (linux/darwin × amd64/arm64)
- `go test -race ./internal/sessioncoord ./internal/snapshot/central ./internal/peering ./cmd/gmuxd` from `services/gmuxd` — pass

No live daemon was installed or restarted.

## PR, revisions, review, and merge

- Branch: `feat/active-subagent-budget`
- Final implementation SHA: `1d9016c6b18f575e11c5a24c9f98834e175f5421`
- PR: [#481](https://github.com/gmuxapp/gmux/pull/481), **feat(daemon): cap local gmux-mediated active semantic-agent descendants per root**
- Three independent read-only reviews used exact pushed SHAs and distinct angles: concurrency/state machine; ownership/config/API; adversarial integration/test quality.
- Demonstrated findings remediated by the lead:
  - install-time recheck after reparent/promotion prevents a receipt admitted under one root from overbooking its new root;
  - reconcile removals and version-conditional takeover outcomes now update only committed projection changes;
  - absent-node ownership mutations cannot subtract live descendant counts;
  - transient pre-commit registration failures unclaim the receipt so the ordinary HTTP retry reuses it;
  - receipt cleanup blocks behind lifecycle traffic rather than best-effort `TryLock`, while a phase-2 panic guard unlocks before re-panicking;
  - production-handoff coverage now exercises detached receipt env transfer/unsetting, real register JSON, mounted routes, and real-store bootstrap convergence.
- The reported ordinary `gmux -d -- pi` bypass was withdrawn because settled scope and docs explicitly target all `gmux agent prompt --new` launches, not every generic gmux command launch.
- Delta reviewers reran their reproductions. Final verdicts were integrate; concurrency probes measured zero stranded receipts after the blocking-cleanup fix.
- CI for final SHA: all 11 checks green, including lint/build/test, hot-package race, production container E2E, Playwright E2E, generated/cross-build, PR binaries, pi-latest compatibility, release scripts, and policy checks.
- Merge SHA: `dc2f5855bd42e836eb0787a07cb3fda58365407f`.

## Deferred follow-ups

- Per-root/UI overrides on top of the existing root-keyed index and host-default interface.
- Optional queued admission, if designed separately; this slice exposes no queueing flag.
- Any depth policy, historical launch accounting, prompt-header signaling, peer-distributed quotas, or UI tree/list redesign requires a separate product decision and is intentionally absent.
