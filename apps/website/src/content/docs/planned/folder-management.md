---
title: Folder Management
description: VCS-aware workspace grouping and manual folder curation.
---

> This feature is not yet implemented.

Today, folders appear in the sidebar automatically when sessions start, and the scanner walks all adapter session directories to discover every resumable session across all projects. This works but creates noise and prevents adapters with per-project storage (like OpenCode's SQLite) from integrating cleanly.

This document describes the replacement, delivered in two steps:
1. VCS-aware workspace grouping (automatic, zero-config)
2. Manual folder management (ordering, hiding, the manage folders modal)

## Step 1: Workspace grouping

### The problem

When you work in jj workspaces or git worktrees, each workspace appears as a separate folder in the sidebar. Three workspaces of the same repo produce three folder entries. The user mentally groups them, but gmux doesn't.

### The solution

Sessions gain a new field: `workspace_root`. The runner detects this at launch by walking up from the session's cwd looking for VCS markers. When multiple visible folders share the same workspace root, the frontend collapses them into a single group.

The sidebar displays grouped sessions under the repo name with all sessions flattened:

```
gmux
  Fix auth bug            ← cwd: ~/dev/gmux
  Add adapter             ← cwd: ~/dev/gmux/.grove/teak
  Update docs             ← cwd: ~/dev/gmux
```

Instead of today's split view:

```
gmux
  Fix auth bug
  Update docs
teak
  Add adapter
```

The actual cwd is still visible in the session detail header. The sidebar just doesn't need it for organization.

Folders that don't share a workspace root with any other folder render as standalone entries, same as today. A single-folder "group" is just a folder.

### Detection

The runner (gmux) detects the workspace root at session startup and includes it in the session metadata served via `/meta`. Detection is filesystem-only (stat calls and file reads, no subprocesses):

1. **jj**: walk up from cwd looking for `.jj/` directory. If found, resolve the repo root. For jj workspaces, `.jj/repo` is a path to the shared store; the main workspace's directory is the root. For colocated repos, the `.jj/` and `.git/` directories sit side by side.
2. **git**: walk up from cwd looking for `.git`. If it's a file (git worktree marker), read it to find the main `.git` directory and derive the main worktree path. If it's a directory, that's the repo root.
3. If neither is found, `workspace_root` is empty. The folder stands alone.

### Data flow

**Runner** (`cli/gmux`):
- New field on `session.State`: `WorkspaceRoot string`
- Detected once at startup, before the session is served via `/meta`
- Included in the JSON response alongside `cwd`

**Store** (`services/gmuxd/internal/store`):
- New field on `store.Session`: `WorkspaceRoot string`
- Populated from the runner's `/meta` response during discovery
- Broadcast to frontends via SSE like any other session field

**Frontend** (`apps/gmux-web`):
- New field on the `Session` type: `workspace_root?: string`
- `groupByFolder` updated: when two or more folders share a `workspace_root`, collapse them into a single group using the root's basename as the display name
- Group position in the sidebar is the position of its first member in the current sort order

### What this doesn't change

- The sidebar still auto-shows folders when sessions start (current `syncSessions` behavior)
- The existing folder hide/show mechanism in `sidebar-state.ts` still works
- The scanner and discovery logic are untouched
- No new configuration, no new API endpoints, no new state files

This step is additive: a new field plumbed through existing session structures (runner, store, protocol) and a frontend rendering change.

## Step 2: Manual folder management

Depends on Step 1. Adds server-side folder state, a management modal, and scoped session discovery.

### State

The folder list moves from browser localStorage to a server-side file at `~/.local/state/gmux/folders.json`. This ensures consistency across browsers and devices (including tailscale remote access).

```json
{
  "items": [
    { "path": "/home/mg/dev/gmux" },
    { "path": "/home/mg/dev/gmux/.grove/teak" },
    { "path": "/home/mg/dev/other-project", "hidden": true }
  ]
}
```

A flat ordered list. Each item has a `path` (always absolute, `~` resolved at write time) and an optional `hidden` flag. That's the entire schema. Grouping is derived at render time from session `workspace_root` fields, not stored as configuration.

New installations start with an empty state. The sidebar populates organically as the user launches sessions.

### Auto-add

When a new gmux session starts in a directory:

1. Resolve the path to absolute form.
2. Check if the path exists anywhere in state (visible or hidden). If yes, do nothing. Hidden folders stay hidden.
3. Otherwise, add as a new item at the end of the list, visible.

### Hidden folders

Any folder can be hidden via the manage folders modal. Hidden folders do not appear in the sidebar, even if they have active sessions.

Hiding is sticky: a hidden folder stays hidden when new sessions start in it. The user must explicitly unhide it via the modal.

Hidden folders with active sessions are indicated via a badge on the "Manage folders" button at the bottom of the sidebar. This is the only visible hint that hidden activity exists.

### Deleted folders

When a configured folder is deleted from disk, it is removed from state entirely (visible and hidden). If the folder is later recreated and a session starts in it, normal auto-add rules apply.

### API

**`GET /v1/folders`**: returns the current folder state.

**`PUT /v1/folders`**: replaces the entire folder state. The frontend sends the complete ordered list on every mutation (reorder, hide, unhide, remove). Folder lists are small (tens of items), so full replacement avoids conflict resolution complexity. Updates are broadcast to all connected clients via SSE.

**`POST /v1/folders/add`**: adds a single folder. Used by the auto-add mechanism on session start and the path input in the modal. Rejects duplicates (paths are normalized before comparison, so `~/dev/gmux` and `/home/mg/dev/gmux` are the same).

### Manage folders modal

Opens from a "Manage folders" button at the bottom of the sidebar.

```
┌──────────────────────────────────────────────┐
│  Manage folders                           X  │
│                                              │
│  ≡  ~/dev/gmux                          👁‍🗨   │
│  ≡  ~/dev/gmux/.grove/teak              👁‍🗨   │
│  ≡  ~/dev/other-project                 👁‍🗨   │
│  ▸ Show 1 hidden                             │
│                                              │
│  ┌────────────────────────────────────┐      │
│  │ /path/to/folder                    │      │
│  └────────────────────────────────────┘      │
│                                              │
└──────────────────────────────────────────────┘
```

**Elements:**

- **Drag handles** (`≡`): reorder items in the flat list.
- **Eye-slash** (`👁‍🗨`): hide. Moves the item to the hidden section.
- **Hidden section**: expandable `[Show N hidden]` at the bottom. Hidden items show an eye icon (no slash) to unhide and an `✕` to remove from state permanently. Hidden items are not draggable.
- **Path input**: always visible at the bottom. Type a path, press Enter to add.
- **Active session badge**: items with running sessions show a subtle count indicator.

### Sidebar rendering

The sidebar is a pure function of the folder state and live session data:

```
visibleFolders = state.items.filter(item => !item.hidden)

// Group by workspace_root (from session data)
groups = groupByWorkspaceRoot(visibleFolders, sessions)

// Render: grouped folders show under repo name,
// standalone folders show under their own name
for each group:
  render heading (repo basename or folder basename)
  render all sessions from all folders in group, sorted by time

// Footer
render "Manage folders" button
  with badge if any hidden folders have active sessions
```

No transition state, no imperative show/hide logic. The sidebar re-renders when the state or session list changes.

### Scanner changes

The session file scanner is scoped to configured folders:

**Before**: walk all adapter session root directories, discover every resumable session for every project.

**After**: for each non-hidden folder in the state, check each adapter's session directory for that cwd. Only discover resumable sessions for folders the user has configured.

This scoping makes adapters like OpenCode viable. Instead of requiring a central session directory, the scanner checks `<cwd>/.opencode/opencode.db` for each configured folder.
