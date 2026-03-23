# Changesets

When a PR includes user-facing changes, add a changeset file here.

Create a markdown file with any name (e.g. `fix-resize-perf.md`):

```markdown
---
bump: patch
---

Fixed mobile keyboard autocorrect duplicating text.
```

`bump` (required): `patch`, `minor`, or `major`.

The body is the changelog entry as it should appear on [gmux.app/changelog](https://gmux.app/changelog). Keep it to one or two sentences.

## What happens

1. PR merges with the `.changesets/*.md` file.
2. A workflow opens a **Release PR** that consumes changesets into `changelog.mdx`.
3. Merging the Release PR tags and releases.
