# AGENTS.md

## State discipline

Never add new state without justification. Before adding a field, ask: who owns it, who updates it, and can it be derived from existing state instead? Prefer derivation over storage. New state creates maintenance burden, sync bugs, and lifecycle complexity.

## Releases

Do **not** edit `apps/website/src/content/docs/changelog.mdx` directly; it is managed by the release workflow.

PR titles must follow conventional commits: `feat:`, `fix:`, `docs:`, `ci:`, etc. Only `feat:` and `fix:` PRs trigger releases. Add `!` before the colon for breaking changes (e.g. `feat!: remove old API`).
