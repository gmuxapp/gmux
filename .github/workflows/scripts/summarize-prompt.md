The attached screenshot shows the gmux web UI.

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
(conventional commit format) and its description for context. Summarize
them into a Discord message for the project's community server.

## Format

Output only the summary text, nothing else. No preamble, no title, no
heading, no links, no sign-off, no "here's the summary" intro.

Use Discord markdown. Write in short paragraphs, not bullet points.
Group by change type using inline bold labels, skipping sections that
have no entries: **Breaking**, **Features**, **Fixes**, **Docs**.

Example structure (do not copy the content):

**Features** Short paragraph describing the notable new capabilities.
Related changes covered together in flowing prose.

**Fixes** Another paragraph covering the important bug fixes.

## Guidelines

- Base the summary on what changed for users, not on implementation details.
  The PR descriptions are background context to help you understand the
  change, not content to surface verbatim.
- Be direct, technical, and accurate. Assume readers are developers who use
  the tool daily. No hype, no filler, no calls to action.
- Multiple entries may be part of the same effort; cover them together.
- Focus on the highlights rather than being exhaustive.
