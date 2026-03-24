# Changesets

When a PR includes user-facing changes, add a changeset file here.

## Adding a changeset

Create a markdown file with any name (e.g. `fix-resize-perf.md`):

```markdown
---
bump: patch
---

- **Faster reconnects for long sessions.** Redundant SIGWINCH signals on reconnect
  caused TUI apps to redraw their entire screen. Resizes that don't change the
  terminal dimensions are now skipped.
```

### Frontmatter

- `bump` (required): `patch`, `minor`, or `major`

### Body

Write the changelog entry exactly as it should appear on the
[changelog page](https://gmux.app/changelog). Use the same style as existing
entries: bold lead sentence, then description. Group related bullets under
`###` category headings if needed.

## What happens

1. Your PR merges with the `.changesets/*.md` file.
2. A workflow opens (or updates) a **Release** PR that consumes all pending
   changesets into `changelog.mdx` and deletes the files.
3. Merging the Release PR creates a git tag, which triggers GoReleaser to
   build binaries and publish the GitHub Release.
