# AGENTS.md

## State discipline

Never add new state without justification. Before adding a field, ask: who owns it, who updates it, and can it be derived from existing state instead? Prefer derivation over storage. New state creates maintenance burden, sync bugs, and lifecycle complexity.

## Changesets

PRs with user-facing changes must include a `.changesets/<name>.md` file. Keep the entry to one or two sentences describing what changed for the user. See `.changesets/README.md` for format.
