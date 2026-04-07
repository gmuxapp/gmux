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
short summary for the project's Discord community server.

## Format

Output only the summary text, nothing else. No preamble, no links,
no sign-off, no "here's the summary" intro.

Organize by topic, not by change type. Group related work together
regardless of whether individual PRs were features, fixes, or docs
changes. Use a **bold heading** to introduce each topic, then a few
sentences of prose. Skip minor fixes unless they affect daily usage.

A detailed PR list follows this summary in the release notes, so don't
try to be exhaustive. Highlight what matters: what's new, what works
differently, what users should know. Call out breaking changes clearly.

Use Discord markdown. 2-4 short paragraphs for a typical release.

## Guidelines

- Base the summary on what changed for users, not on implementation details.
  The PR descriptions are background context to help you understand the
  change, not content to surface verbatim.
- Be direct, technical, and accurate. Assume readers are developers who use
  the tool daily. No hype, no filler, no calls to action.
- Related PRs are often part of the same effort; cover them once.

