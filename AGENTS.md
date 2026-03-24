# AGENTS.md

## State discipline

Never add new state without justification. Before adding a field, ask: who owns it, who updates it, and can it be derived from existing state instead? Prefer derivation over storage. New state creates maintenance burden, sync bugs, and lifecycle complexity.

## Changelog

Do **not** edit `apps/website/src/content/docs/changelog.mdx` directly; it is managed by the release workflow. Instead, add a `.changesets/<name>.md` file (see `.changesets/README.md` for format). The release workflow consumes changesets into the changelog and deletes the files.
