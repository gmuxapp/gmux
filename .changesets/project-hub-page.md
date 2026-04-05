---
bump: minor
---

- **Project hub page.** Clicking a project name in the sidebar (or
  navigating to `/:project`) now shows a project overview instead of
  auto-opening the first session. The hub groups that project's sessions
  by host and working directory, with breadcrumb headers that show the
  full topology chain for nested hosts (e.g. `workstation › alpine-dev`
  for a devcontainer running on a remote peer). Each folder row has a
  `+` launcher so you can spin up a new session right where you need
  it. The sidebar folder name is highlighted while its hub is open.
- **Home page via the gmux logo.** Clicking the "gmux" logo in the
  sidebar now returns to `/` where you can launch a new session. Visiting
  `/` no longer auto-redirects into the first running session.
