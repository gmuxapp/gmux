---
name: gmux-agent
description: Launch and orchestrate AI agents through gmux sessions. Use when the user asks to spawn a subagent, delegate a task or investigation to an agent, run agents in parallel, send an agent further instructions, or check an agent's answer.
---

An agent launched through gmux becomes a session the user can watch live while you drive it semantically. Details: `gmux agent ... --help`.

## Core flow

```bash
gmux agent prompt --new 'Fix the auth bug in ...'   # id on stderr, exchange report on stdout
gmux agent prompt a1b2c3d4 'Add tests and ...'
```

## Parallel execution

```bash
id1=$(gmux agent prompt --new --no-wait 'Review ...')
id2=$(gmux agent prompt --new --no-wait 'Check concurrency ...')

gmux wait "$id1" "$id2"   # prints results when all are done
```

Prefer creating a new worktree for each agent following the project's established conventions. This can be useful even for review agents, so they can modify code and observe changes in behavior.

## Notes

- Read the last N exchanges: `gmux agent logs a1b2c3d4 [-n N]`
- Prompt text can come from stdin: `gmux agent prompt a1b2c3d4 < instructions.md`
- Interrupting `prompt` and `wait` commands keeps the agent running in the background. Launch a new `wait` to monitor progress, or interrupt explicitly: `gmux agent cancel a1b2c3d4`. Note that the agent may have taken actions (like edits) before the interrupt.
- Always use `gmux` commands when waiting on an agent. (Instead of polling files for example.)
