# AGENTS.md

## Correctness

Prioritize verifiable, correct behavior above all else. If something has lots of race conditions or other edgecases that are difficult to test and reason about, it is likely the wrong approach. If a library does something more reliably than what we can achieve, it is worth considering.

## State discipline

Never add new state without justification. Before adding a field, ask: who owns it, who updates it, and can it be derived from existing state instead? Prefer derivation over storage. New state creates maintenance burden, sync bugs, and lifecycle complexity.

## Peering model

Hub-and-spoke: each node's SSE stream only includes sessions it **owns** (local + devcontainer). Network peer sessions are excluded. `PeerConfig.Local` distinguishes the two: only the Docker watcher sets it. Tailscale-discovered and manual peers are not Local.

## Commits and releases

Every commit on `main` is changelog material: the release pipeline
(`version.sh` + [git-cliff](https://git-cliff.org/)) reads commit
messages directly. Rules that follow from this:

- **Every commit is a conventional commit**, not just PR titles. Use
  `feat:`, `fix:`, `docs:`, `perf:`, `security:`, `refactor:`,
  `chore:`, `ci:`, `test:`, `style:`, `build:`. `feat!:` or
  `BREAKING CHANGE:` footer marks a major bump.
- **Scopes are optional but encouraged** for monorepo areas: `web`,
  `daemon`, `cli`, `adapter`, `peering`, `devcontainer`, `docs`.
  Example: `feat(peering): reconnect after system sleep`. Scopes show
  up as bold tags in the changelog: `- **(peering)** reconnect after
  system sleep`. The `release` scope is reserved for release-flow
  plumbing (workflows, notify-discord, cliff.toml itself) and is
  hidden from the changelog and bump computation: those changes
  affect maintainers, not users. Breaking release-scope changes
  (`feat(release)!:`) still surface so consumers of the release
  pipeline know to act.
- **Write commit messages as user-facing changelog bullets.** The text
  after `type: ` becomes the bullet verbatim. Lowercase, no trailing
  period, imperative mood. Good: `fix: prevent recursive config fetch
  storm`. Bad: `fix: Fixed the config storm issue.`.
- **Release behavior by type**: `feat` bumps minor, `fix` / `perf` /
  `security` bumps patch, `feat!` / `fix!` / `BREAKING CHANGE:` bumps
  major. `docs` appears in the changelog but doesn't trigger a
  release on its own. Everything else is hidden.
- **Security fixes** use `security:` (or `security!:` for breaking
  security changes) and appear in their own `### Security` section at
  the top of the release, right after Breaking.
- **PRs use rebase merge**, not squash. Atomic commits on feature
  branches land on `main` as-is, so keep them clean before pushing. Use
  `jj squash` / `jj split` / `jj describe` to fix up WIP commits
  locally.
- **Prose highlights for a release** live in the open `release/next`
  PR body, between the `<!-- prose-start -->` and `<!-- prose-end -->`
  markers. The PR body is the single source of truth: edit it
  directly in the GitHub UI, and the regen workflow re-syncs
  `changelog.mdx` and the bullets section of the body on every edit
  (and on every push to `main`). There is no `RELEASE_HIGHLIGHTS.md`
  file and no script to run after editing. Leave the prose section
  empty for patch-only releases that don't need curated prose; the
  Discord announcement falls back to the auto-generated bullet list
  so subscribers can still see what changed without clicking through.

## Killing processes

Tests, probes and cleanup scripts run on real developer desktops, not in
throwaway containers. A stray signal takes down the user's session.

- **Never derive a signal target from a process-name pattern.** `pgrep -f
  'sleep 30'` matches other people's processes, including ones owned by the
  graphical session. Signal only PIDs that the test or script recorded when it
  spawned them.
- **Never signal a negative PID unless we created that process group.** Verify
  `getpgid(pid) == pid` first: gmux detaches runners with `setsid`, so a leader
  we own is its own group. A group id read out of `ps` may be the compositor's,
  and `kill -- -$pgid` then takes the whole desktop with it.
- **Escalate inside our own tree only**: TERM the recorded PID or a verified
  group, poll for disappearance, then KILL. Never `kill -1`, never `kill 0`.
- **Isolate destructive process work** in a container or a separate login
  session. Isolated `XDG_*` directories scope files, not signals.

Precedent: a review agent reaped a leaked `sleep 30` by pattern, matched the
unrelated `sleep 30` polling inside `pi-sleep-guard`, and TERM+KILLed its
process group — which was the compositor's. The desktop died and the user was
returned to the login screen.

## Other rules

- Push changes and create pull requests. Don't commit directly to
  `main`.
- Use `./scripts/install.sh` when asked to install locally.
