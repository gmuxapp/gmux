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
  system sleep`.
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
- **Prose highlights for a release** go in `RELEASE_HIGHLIGHTS.md` at
  the repo root. Edit it as part of the PR that ships the noteworthy
  change. The content is injected into the changelog section between
  the version heading and the grouped bullet lists, and cleared after
  release.

## Other rules

- Push changes and create pull requests. Don't commit directly to
  `main`.
- Use `./scripts/install.sh` when asked to install locally.
