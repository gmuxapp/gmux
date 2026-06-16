# agent-shim

A tiny, readable preload that gmux injects into node/bun-based agent
processes (pi, claude, codex — anything that persists a JSONL session file)
so gmux learns **authoritatively** which conversation file a runner holds,
the moment the agent writes it. This replaces post-hoc scrollback matching.

See `hook.mjs` for the full design comment. Short version:

- The runner spawns the agent with (append-safe):
  - node: `NODE_OPTIONS="… --import file:///abs/hook.mjs"`
  - bun:  `BUN_OPTIONS="… --preload /abs/hook.mjs"`
  - plus `GMUX_RUNNER_SOCK=<runner unix socket>`
- The shim wraps the fs **write** surface (`openSync`/`appendFileSync`/
  `writeFileSync`, with fd→path tracking) and POSTs `{op,path,pid,data,bytes}`
  to `POST /shim/event` on the runner socket whenever a `*.jsonl` file is
  written. The runner forwards to the daemon; the adapter turns the delta
  into gmux events.
- **Reads are not reported.** Typing `/resume` makes the agent readdir +
  bulk-read every session file for its picker — pure noise. A real
  resume/rebind always ends in a *write* to the chosen file, so writes alone
  catch attribution and rebind.
- It arms only when `GMUX_RUNNER_SOCK` is set, then deletes that var and the
  injected `*_OPTIONS` from `process.env`, so any child the agent spawns
  (npm, sub-node, sub-bun) disarms itself.

## Testing against a real agent

```sh
# interactive: launch pi under node, watch shim events on stderr
node test-harness.mjs -- pi

# under bun
node test-harness.mjs --bun -- pi
```

Write a message, then `/resume` a different session, and watch the rebind
report (a write to the newly chosen `*.jsonl`) arrive.

## Wiring (runner side)

`agentshim.go` embeds `hook.mjs` and exposes:
- `Path()` — materializes the shim to a content-addressed file under the
  user cache dir and returns its path (readable on disk; `--import`/
  `--preload` need a real path).
- `PreloadEnv(env, shimPath, sockPath)` — append-safe injection of
  `NODE_OPTIONS`/`BUN_OPTIONS` + `GMUX_RUNNER_SOCK`.

`ptyserver.New` calls these for any adapter implementing
`adapter.SessionShimmer` (pi today; claude/codex are one-liners). The
`POST /shim/event` handler records the reported path via
`session.State.SetSessionFile`, which emits a `session_file` event on the
existing `/events` stream on first-attribution and on rebind.

## Status

Reference implementation. Verified on node 26 and bun 1.3:
- pi's exact write pattern (`openSync(path,"wx")`+`writeFileSync(fd,…)` flush,
  then `appendFileSync` deltas) is captured on both runtimes;
- node's `appendFileSync`→`writeFileSync` re-entrancy is de-duplicated;
- the `/resume` picker's bulk reads are correctly ignored;
- spawned child processes are disarmed.

Runner side is wired (env injection, `POST /shim/event`, `session_file`
event on `/events`) and covered by an end-to-end test that drives a real
node agent through `ptyserver`. Still to do: the **daemon** `ShimAttributor`
that consumes the `session_file` event to set attribution authoritatively
and bypass scrollback matching, plus runner re-announce on daemon reconnect
so `attributions.json` can be retired.
