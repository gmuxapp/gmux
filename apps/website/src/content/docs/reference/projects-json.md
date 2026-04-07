---
title: projects.json
description: Reference for ~/.local/state/gmux/projects.json — project definitions and match rules.
tableOfContents:
  maxHeadingLevel: 3
---

`~/.local/state/gmux/projects.json` (or `$XDG_STATE_HOME/gmux/projects.json`)

Projects control which sessions appear in the sidebar and how they're grouped. gmuxd reads and writes this file. You can also edit it directly; changes are picked up on the next daemon restart. The UI's **Manage projects** modal is the primary editing interface.

## Example

```json
{
  "version": 2,
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
      "match": [
        { "path": "/data/ml", "hosts": ["gpu-server"] }
      ]
    }
  ]
}
```

## Top-level fields

| Field | Type | Description |
|-------|------|-------------|
| `version` | `number` | Schema version. Currently `2`. Managed by gmuxd; do not change manually. |
| `items` | `Item[]` | Ordered list of projects. Order matters: it controls sidebar display order and tiebreaking for remote matches. |

## Item fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `slug` | `string` | yes | URL-safe identifier. Lowercase alphanumeric and hyphens, no leading/trailing hyphen. Appears in URLs (`/:slug/:adapter/:session`). |
| `match` | `MatchRule[]` | yes | One or more rules that determine which sessions belong to this project. At least one rule is required. |
| `sessions` | `string[]` | no | Session keys (resume keys or IDs) assigned to this project. Managed by gmuxd; preserved but not required when editing. |

## Match rules

Each rule is either a **path rule** or a **remote rule**. A rule cannot have both `path` and `remote`.

| Field | Type | Description |
|-------|------|-------------|
| `path` | `string` | Filesystem path. Sessions whose working directory is at or under this path match. Paths starting with `~/` are expanded to `$HOME`. |
| `remote` | `string` | Normalized git remote URL (e.g. `github.com/org/repo`). Sessions whose repository has a matching remote match regardless of filesystem path. |
| `hosts` | `string[]` | Restrict this rule to sessions from specific peer hosts. Empty or absent means any host. |
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

### Host scoping

Rules can be restricted to sessions from specific [peer hosts](/multi-machine):

```json
{ "path": "/data/ml", "hosts": ["gpu-server"] }
```

This rule only applies to sessions running on the peer named `gpu-server`. Local sessions and sessions from other peers are not affected. Rules without `hosts` match sessions from any host.

## Match precedence

When multiple projects could match a session:

1. **Path specificity**: the project with the longest matching path wins. A session in `~/dev/gmux/.grove/teak/src` matches `~/dev/gmux/.grove/teak` over `~/dev/gmux`.
2. **Path over remote**: a path match always takes priority over a remote match.
3. **First remote wins**: when only remote rules match, the first matching project in list order wins. Drag to reorder projects in the manage modal to control this.

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

**Host-specific rules** for the same project on different machines:
```json
{
  "slug": "ml",
  "match": [
    { "path": "~/ml", "hosts": ["laptop"] },
    { "path": "/data/ml", "hosts": ["gpu-server"] }
  ]
}
```

## Validation

gmuxd validates the file on load. Invalid state is rejected with an error. Rules:

- Every item must have a non-empty `slug` matching `^[a-z0-9]+(-[a-z0-9]+)*$`
- No duplicate slugs
- Every item must have at least one match rule
- Each rule must have exactly one of `path` or `remote` (not both, not neither)
- `exact` is only valid on path rules
- No duplicate normalized paths across items (nesting is allowed)

## Migration

Older projects.json files (version 1 or unversioned) are migrated automatically on load. The migration converts the old `remote`/`paths` fields to `match` rules and canonicalizes paths under `$HOME` to `~/...` form. The migrated version is written back on the next save.
