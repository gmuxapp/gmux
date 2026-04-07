## About gmux

gmux is a session manager for AI coding agents (and any terminal command).
Users launch commands with `gmux <cmd>`, which creates managed terminal
sessions grouped by project. A live browser dashboard shows all sessions
with real-time status, and works from desktop or phone. The architecture
is two binaries: `gmux` (per-session PTY + WebSocket) and `gmuxd`
(per-machine daemon for discovery and proxying).

The audience is developers who run multiple AI coding agents, test watchers,
and build processes across their machines.

## Task

Below are the merged PRs for a release. Each entry has a PR title
(conventional commit format) and its description for context. Write a
brief summary for the project's Discord community server.

## Format

Output only the summary, nothing else. No preamble, no links, no
sign-off.

Use ### subheadings to separate topics, with 1-2 sentences each.
Organize by topic, not by change type. Group related work together.
Skip minor fixes unless they affect daily usage. The full PR list is
right below this summary, so keep it high-level.

Aim for the whole summary to be around 100 words. Treat that as a
soft target, not a hard limit.

Example tone and length:

### Project matching
`projects.json` uses a new `match` rule array instead of separate
`remote`/`paths` fields. Supports exact matching and per-host scoping.
Existing configs migrate automatically.

### Sleep recovery
Peers now automatically reconnect after system suspend, no restart needed.

## Guidelines

- Write for users, not implementers. Skip internal details.
- Be direct and technical. No hype, no filler.
- Related PRs are often part of the same effort; cover them once.
- Flag breaking changes with **Breaking:** at the start of the sentence.
