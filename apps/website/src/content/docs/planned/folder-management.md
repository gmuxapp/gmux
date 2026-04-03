---
title: Project Management
description: User-curated project list with VCS-aware matching and stable URL routing.
---

> This feature is not yet implemented.

Today, folders appear in the sidebar automatically when sessions start, and the scanner walks all adapter session directories to discover every resumable session across all projects. This works but creates noise, doesn't sync between clients, and prevents adapters with per-project storage (like OpenCode's SQLite) from integrating cleanly.

This document describes the replacement, delivered in three steps:
1. Server-side project state with user-curated project list
2. Manage projects UI
3. URL routing (stable, hierarchical session URLs)

## Design principles

**Projects, not folders.** The primary unit of organization is a project, not a filesystem path. A project is "gmux" or "my-api", not `/home/mg/dev/gmux`. Folders are an implementation detail of where code lives; the project is what the user thinks about.

**Nothing auto-added.** Sessions are never automatically added to the sidebar. Instead, gmuxd discovers active sessions and offers them as potential projects. The user explicitly adds projects they care about. This gives the user full control and avoids the "whack-a-mole hiding" problem.

**Synced between clients.** Project state lives server-side, so phone, laptop, and tailscale remote all see the same sidebar. The state is owned by the gmuxd instance serving the web UI (the "home" instance in a multi-host setup).

## Step 1: Server-side project state

### What is a project?

A project is a user-configured entry that matches sessions to a named group in the sidebar. Each project has:

- A **slug** (URL-safe identifier, e.g. `gmux`)
- One or more **paths** (where the code lives on disk, used for launching sessions)
- An optional **remote URL** (when set, matching uses the remote instead of paths)

### Match rules

Every project has filesystem paths. If a project also has a remote URL, matching uses the remote; otherwise matching uses the paths.

**Remote-matched:** The project stores a normalized remote URL (e.g. `github.com/gmuxapp/gmux`). A session matches if any of its remotes equals this URL. This handles forks (origin vs upstream), multiple clones, cross-machine grouping, and worktrees automatically. The paths are still stored for context (launch directory, display) but are not used for matching.

**Path-matched:** The project has no remote URL. A session matches if its cwd or workspace_root is under one of the project's paths (prefix match). This handles local-only repos, scratch directories, and intentional carve-outs from remote-matched projects.

### Match precedence

When a session could match multiple projects, the most specific match wins:

1. **Path-matched projects, longest prefix first.** A project claiming `/home/mg/dev/gmux/.grove/teak` beats one claiming `/home/mg/dev/gmux` for sessions in the teak directory.
2. **Remote-matched projects.** Checked only if no path-matched project matched.
3. **Unmatched sessions** are not shown in the sidebar. They appear in the "discovered" list as candidates for the user to add.

Two projects claiming the exact same normalized path is a validation error. Nesting (one path under another) is valid and intentional, with longest prefix winning.

### State file

Stored at `~/.local/state/gmux/projects.json`:

```json
{
  "items": [
    { "slug": "gmux", "remote": "github.com/gmuxapp/gmux", "paths": ["/home/mg/dev/gmux"] },
    { "slug": "teak", "paths": ["/home/mg/dev/gmux/.grove/teak"] },
    { "slug": "scripts", "paths": ["/home/mg/scripts"] }
  ]
}
```

Every item has a `slug` and `paths`. Items with a `remote` match by remote URL; items without match by paths. The array order is the display order.

New installations start with an empty list. The sidebar shows "no projects configured" with a prompt to add one.

### Discovery (offered projects)

gmuxd always knows about active sessions via socket discovery. Sessions that don't match any configured project are grouped using the existing union-find logic (shared remotes, then shared workspace_root, then shared cwd) and presented as "discovered" projects.

The `GET /v1/projects` response includes both:

```json
{
  "configured": [
    { "slug": "gmux", "remote": "github.com/gmuxapp/gmux" }
  ],
  "discovered": [
    {
      "suggested_slug": "other-project",
      "remote": "github.com/someone/other-project",
      "paths": ["/home/mg/dev/other-project"],
      "session_count": 3
    }
  ]
}
```

Discovered entries include both the remote (if available) and paths so the UI can show context. When the user adds one, the match rule is chosen automatically: remote if available, paths otherwise. The user can override this in the manage UI.

### API

**`GET /v1/projects`**: returns configured projects and discovered (unmatched) session groups.

**`PUT /v1/projects`**: replaces the entire project list. The frontend sends the complete ordered list on every mutation (reorder, remove, rename). Project lists are small, so full replacement avoids conflict resolution. Validates: no duplicate paths after normalization, every item has paths. Broadcasts `projects-update` SSE event to all clients.

**`POST /v1/projects/add`**: adds a single project. Used by the "add project" flow. Derives the slug from the remote URL repo name or directory basename. Rejects duplicate match rules. Broadcasts SSE.

### Scanner scoping

The session file scanner is scoped to configured projects:

**Before**: walk all adapter session root directories, discover every resumable session for every project.

**After**: for each configured project, check each adapter's session directory for that project's paths. Only discover resumable sessions for projects the user has configured.

This scoping makes adapters like OpenCode viable. Instead of requiring a central session directory, the scanner checks `<path>/.opencode/opencode.db` for each configured project path.

## Step 2: Manage projects UI

Opens from a "Manage projects" button at the bottom of the sidebar (or prominently in the empty state).

### Empty state

First-time users see:

```
┌──────────────────────────────────────────────┐
│                                              │
│          No projects configured              │
│                                              │
│  We found active sessions in:                │
│                                              │
│  ┌ gmux ─────────────────────── [Add] ┐     │
│  │ github.com/gmuxapp/gmux            │     │
│  │ 3 active sessions                  │     │
│  └─────────────────────────────────────┘     │
│                                              │
│  ┌ scripts ──────────────────── [Add] ┐     │
│  │ ~/scripts                          │     │
│  │ 1 active session                   │     │
│  └─────────────────────────────────────┘     │
│                                              │
│  ┌────────────────────────────────────┐      │
│  │ /path/to/project              [+] │      │
│  └────────────────────────────────────┘      │
│                                              │
└──────────────────────────────────────────────┘
```

### Manage modal

```
┌──────────────────────────────────────────────┐
│  Manage projects                          X  │
│                                              │
│  ≡  gmux                                 ✕   │
│     github.com/gmuxapp/gmux                  │
│  ≡  teak                                 ✕   │
│     ~/dev/gmux/.grove/teak                   │
│                                              │
│  ── Discovered ──────────────────────────    │
│  other-project (2 sessions)        [Add]     │
│                                              │
│  ┌────────────────────────────────────┐      │
│  │ /path/to/project              [+] │      │
│  └────────────────────────────────────┘      │
│                                              │
└──────────────────────────────────────────────┘
```

**Elements:**

- **Drag handles** (`≡`): reorder projects in the sidebar.
- **Remove button** (`✕`): removes the project. Sessions become discoverable again.
- **Discovered section**: shows unmatched session groups with an Add button.
- **Path input**: type or paste a path to add a project manually.
- **Active session count**: subtle indicator per project.

### Sidebar rendering

The sidebar is a pure function of the project state and live session data:

```
for each project in state.items:
  sessions = allSessions.filter(s => project.matches(s))
  render project heading (project slug)
  render all matched sessions, sorted by time
  launch button uses project.paths[0] as cwd

footer:
  render "Manage projects" button
```

No transition state, no imperative show/hide logic. The sidebar re-renders when the state or session list changes.

## Step 3: URL routing

Depends on Step 1. Adds hierarchical, stable URLs for every session.

### URL structure

Each session is addressable at:

```
/<project>/<adapter>/<slug>
```

Examples:

```
/gmux/pi/fix-auth-bug
/gmux/shell/pytest-watch
/scripts/shell/backup-3
```

Each segment is meaningful:

- **project**: the project slug from the project configuration.
- **adapter**: the session's `kind` (`pi`, `claude`, `shell`, `codex`, etc.). Each adapter gets its own namespace within a project, so adapters don't need to coordinate slug uniqueness with each other.
- **slug**: an adapter-provided stable identifier for the session. See below.

### Session slugs

Adapters provide a `slug` field via the existing child protocol (`/meta` response or `GMUX_SOCKET` HTTP). The slug is:

- Derived from something stable in the adapter's domain: pi uses its conversation ID or first-message summary, Claude uses the session file basename, shell uses a sanitized command name or counter.
- Unique within the adapter's namespace for that project. If the adapter produces a duplicate, gmux appends a disambiguator (e.g. `-2`).
- Falls back to a truncated session ID (e.g. `sess-abc12`) if the adapter doesn't provide one.

The slug is stable across kill and resume. A resumed session keeps the same slug because the adapter's stable identifier (conversation ID, session file) doesn't change. The internal session ID and process may change, but the URL-facing slug stays the same.

This makes URLs bookmarkable and shareable. A link to `/gmux/pi/fix-auth-bug` resolves to the same logical session regardless of how many times it has been resumed.

See [Session Schema](/develop/session-schema) for the `slug` field definition.

### Project slugs

The project slug comes directly from the project configuration. It's set when the project is created (derived from the repo name or directory basename) and user-editable. Renaming a project slug changes its URLs.

Project slugs must be unique across all configured projects. The `PUT /v1/projects` endpoint validates this.

### Frontend routing

The frontend uses real URL paths (not hash fragments or query params). The existing `preact-iso` router handles this with path patterns:

- `/:project/:adapter/:slug` selects a specific session
- `/:project` shows the project, selects first session
- `/` shows the default view

Navigating to a session updates the URL bar. Clicking a session in the sidebar pushes a new URL. The browser's back/forward buttons work. External links open the correct session directly.

### Forward compatibility with aggregation

When [peer aggregation](/planned/peer-discovery-aggregation) is added, a host prefix slots in before the project:

```
/gmux/pi/fix-auth-bug          → local session
/desktop/gmux/pi/fix-auth-bug  → session on the desktop spoke
```

Existing local URLs continue to work. The project state (which projects to show, ordering, visibility) is owned by the hub gmuxd; spokes just serve sessions.

### What this enables

- **Deep linking**: notification actions link to the specific session that finished.
- **Bookmarks**: pin a long-running session in your browser.
- **External tools**: CI can open `https://gmux.tailnet.ts.net/myproject/shell/build` to show a build session.
- **Aggregation-ready**: the URL structure extends naturally with a host prefix.
