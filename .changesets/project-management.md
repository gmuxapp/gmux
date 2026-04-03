---
bump: minor
---

### Project management

- **Projects replace auto-showing folders.** The sidebar no longer
  automatically adds every directory that has sessions. Instead, gmuxd
  discovers active session groups and offers them in an "Add project" picker.
  You explicitly choose which projects appear in your sidebar.
- **Server-side project state.** Project configuration is stored in
  `~/.local/state/gmux/projects.json` and synced to all connected clients
  via SSE. Phone, laptop, and Tailscale remote all see the same sidebar.
- **Two matching modes.** Every project has filesystem paths (where the
  code lives). Projects can optionally match by remote URL for cross-machine
  grouping. Path-matched projects take precedence, with longest prefix
  winning, so you can carve out a subdirectory as its own project.
- **Session slugs for stable URLs.** Every session gets a URL-safe slug
  derived from its resume key, command, or adapter-provided identifier.
  Slugs are unique per adapter kind and stable across kill/resume.
- **Hierarchical URL routing.** Sessions are addressable at
  `/<project>/<adapter>/<slug>`. URLs update as you navigate, work with
  browser back/forward, and are bookmarkable.
