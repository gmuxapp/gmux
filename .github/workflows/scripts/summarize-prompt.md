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

## Guidelines

- Base the summary on what changed for users, not on implementation details.
  The PR descriptions are background context to help you understand the
  change, not content to surface verbatim.
- Be direct, technical, and accurate. Assume readers are developers who use
  the tool daily. No hype, no filler, no calls to action.
- Group by change type, skipping empty sections: breaking changes first,
  then features, then fixes.
- Multiple entries may be part of the same effort; cover them once.
- Focus on the highlights rather than being exhaustive.
- Use Discord markdown. Use - for bullet points.
- Do not include a title, heading, links, or sign-off.
- Prefer 2-4 sentences per section. One bullet per logical change.
