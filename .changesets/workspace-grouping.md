---
bump: minor
---

- **Workspace-aware sidebar grouping.** Sessions in jj workspaces or git
  worktrees that share the same repository are now grouped under a single
  folder in the sidebar. For example, sessions running in
  `~/dev/gmux/.grove/teak` and `~/dev/gmux` collapse into one "gmux" group
  instead of appearing as separate folders.
