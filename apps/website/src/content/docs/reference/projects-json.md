---
title: projects.json
description: Reference for ~/.local/state/gmux/projects.json — project definitions and match rules.
tableOfContents:
  maxHeadingLevel: 3
---

`~/.local/state/gmux/projects.json` (or `$XDG_STATE_HOME/gmux/projects.json`)

Projects control which sessions appear in the sidebar and how they're grouped. gmuxd reads and writes this file. You can also edit it directly — gmuxd re-reads the file on every access, so manual edits take effect immediately (open UIs won't refresh until the next change made through gmux, and a daemon-side mutation racing your edit will overwrite it). **Settings → Projects** is the primary editing interface. gmuxd seeds a default `home` project when the file is empty or missing.

## Example

```json
{
  "version": 3,
  "items": [
    {
      "slug": "home",
      "match": [
        { "path": "~", "exact": true }
      ]
    },
    {
      "slug": "gmux",
      "match": [
        { "remote": "github.com/gmuxapp/gmux" },
        { "path": "~/dev/gmux" }
      ],
      "sessions": ["fix-auth", "sess-a1b2c3d4"]
    },
    {
      "slug": "ml-data",
      "peer": "gpu-server",
      "node_id": "n-3f9c…"
    }
  ]
}
```

## Top-level fields

| Field | Type | Description |
|-------|------|-------------|
| `version` | `number` | Schema version. Currently `3`. Managed by gmuxd; do not change manually. |
| `items` | `Item[]` | Ordered list of projects. Order matters: it controls sidebar display order and tiebreaking for remote matches. |

## Item fields

An item is either an **owned project** (`slug` + `match`, optionally `sessions`) or a **peer reference** (`slug` + `peer`, optionally `node_id`). References carry no `match` or `sessions` — the peer's own `projects.json` is the source of truth.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `slug` | `string` | yes | URL-safe identifier. Lowercase alphanumeric and hyphens, no leading/trailing hyphen. Appears in URLs (`/:slug/:adapter/:session`). |
| `match` | `MatchRule[]` | owned items | One or more rules that determine which sessions belong to this project. Required on owned items; forbidden on references. |
| `sessions` | `string[]` | no | Session keys (the session's slug when attributed, otherwise its ID; devcontainer sessions are suffixed `@<peer>`) assigned to this project. Managed by gmuxd; owned items only. |
| `peer` | `string` | references | The display name of the peer host that owns this project. Its presence makes the item a reference. |
| `node_id` | `string` | no | References only. Stable opaque identity of the referenced peer; keeps the reference anchored to the right host across removes/re-adds. Stamped by gmuxd. |

## Match rules

Each rule is either a **path rule** or a **remote rule**. A rule cannot have both `path` and `remote`.

| Field | Type | Description |
|-------|------|-------------|
| `path` | `string` | Filesystem path. Sessions whose working directory is at or under this path match. Paths starting with `~/` are expanded to `$HOME`. |
| `remote` | `string` | Normalized git remote URL (e.g. `github.com/org/repo`). Sessions whose repository has a matching remote match regardless of filesystem path. |
| `exact` | `boolean` | Path rules only. When `true`, only sessions whose working directory equals the path exactly match. Subdirectories do not match. |

### Path rules

A path rule matches sessions whose `cwd` or `workspace_root` is at or under the given path. Paths are stored in canonical form (`~/...` for anything under `$HOME`). The server expands `~` to the actual home directory before comparing.

```json
{ "path": "~/dev/gmux" }
```

This matches sessions in `~/dev/gmux`, `~/dev/gmux/src`, `~/dev/gmux/.grove/teak`, etc.

### Remote rules

A remote rule matches sessions by git remote URL, independent of filesystem path. URLs are normalized: protocol prefixes, `.git` suffixes, and SSH user prefixes are stripped.

```json
{ "remote": "github.com/gmuxapp/gmux" }
```

All of these session remotes would match: `https://github.com/gmuxapp/gmux.git`, `git@github.com:gmuxapp/gmux.git`, `ssh://git@github.com/gmuxapp/gmux`.

### Exact matching

By default, path rules match subdirectories. Set `exact: true` to match only the exact directory:

```json
{ "path": "~", "exact": true }
```

This matches sessions started from `$HOME` itself, but not `~/dev/gmux` or any other subdirectory. The default "home" project uses this to avoid catching every session.

## Peer references

Match rules are local to the host that owns the project. Another host's project is pinned into your sidebar as a **reference item** (`{ "slug": …, "peer": …, "node_id": … }`); its match rules and session order live in that peer's own `projects.json`. See [Multi-machine](/multi-machine/) for how references resolve and recover.

The pre-2.0 `hosts` match-rule field is gone: it decodes silently for compatibility and is dropped the next time the file is saved. Host scoping is now implicit in ownership — each project is owned by exactly one host, and other hosts see it via a reference.

## Match precedence

When multiple projects could match a session:

1. **Path specificity**: the project with the longest matching path wins. A session in `~/dev/gmux/.grove/teak/src` matches `~/dev/gmux/.grove/teak` over `~/dev/gmux`.
2. **Path over remote**: a path match always takes priority over a remote match.
3. **First remote wins**: when only remote rules match, the first matching project in list order wins. Drag to reorder projects in **Settings → Projects** to control this.

## Combining rules

A project can have multiple rules for flexibility. Common patterns:

**Remote + path** for cross-machine projects:
```json
{
  "slug": "gmux",
  "match": [
    { "remote": "github.com/gmuxapp/gmux" },
    { "path": "~/dev/gmux" }
  ]
}
```

The remote catches sessions in any clone on any machine. The path catches sessions that haven't pushed yet or have a different remote (e.g. a fork).

**Multiple paths** for monorepos or scattered directories:
```json
{
  "slug": "infra",
  "match": [
    { "path": "~/ops/terraform" },
    { "path": "~/ops/ansible" }
  ]
}
```

## Validation

gmuxd validates the file on load. A file that can't be parsed as JSON is a hard error, but individual items that fail the rules below are **dropped** on load (each logged) rather than rejecting the whole file — one hand-edited bad entry can't poison the roster and block every later mutation. The original bytes are snapshotted to `projects.json.bak` before the sanitized form is written back on the next save. Rules:

- Every item must have a non-empty `slug` matching `^[a-z0-9]+(-[a-z0-9]+)*$`
- No duplicate slugs among owned projects; no duplicate `peer`+`slug` pairs among references (an owned project and a peer reference may share a slug)
- Every owned item must have at least one match rule; references must not carry `match` or `sessions`
- `node_id` is only valid on references
- Each rule must have exactly one of `path` or `remote` (not both, not neither)
- `exact` is only valid on path rules
- No duplicate normalized paths across items (nesting is allowed)

All API mutations are validated the same way, but here the whole mutation is rejected (4xx) and nothing is written — the drop-invalid-items repair applies only to what's already on disk at load time.

## Migration

Older projects.json files are migrated automatically on load: unversioned files' `remote`/`paths` fields become `match` rules (paths canonicalized to `~/...` form), and version-2 files pass through with the removed `hosts` field dropped. Before any schema upgrade rewrites the file, the pre-migration bytes are snapshotted to `projects.json.bak`. The migrated version is written back on the next save.
