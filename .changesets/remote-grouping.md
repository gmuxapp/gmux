---
bump: minor
---

- **Smarter project grouping.** Sessions are now grouped by shared VCS remote URLs instead of filesystem paths. Two clones of the same repo on different machines (or with different directory names) appear under one project heading. Fork workflows just work: if your fork's `origin` and the upstream repo share any remote URL, they group together. Falls back to workspace root and directory path for repos without remotes.
