# gmux `wait` neutral-language report

## Result

- Branch: `fix/wait-neutral-language`
- PR: #479 — https://github.com/gmuxapp/gmux/pull/479
- Implementation commit SHA: `8926a9947f2157589261760f026efc19ecfbdc5f`

## Before / after

Ordinary command and process sessions previously entered the agent exchange renderer for every non-predicate conclusion:

| Outcome | Before | After |
| --- | --- | --- |
| completed without an exchange frame | `[No exchanges yet]` | `[Session activity completed]` |
| intentionally interrupted | `[No exchanges yet]` + `[Agent interrupted]` | `[Session activity interrupted]` |
| failed | `[No exchanges yet]` + `[Agent failed: …]` | `[Session activity failed: …]` |
| authoritative timeout | `[No exchanges yet]` + `[Wait timed out after Ns; agent active, no completed iterations yet...]` | `[Wait timed out after Ns; session remains active]` |
| local SIGINT/SIGTERM | `[Wait interrupted; agent remains active]` | `[Wait interrupted; session activity continues]` |
| generic runner loss | `[Agent failed: agent activity was lost]` | agent waits retain that wording; ordinary waits use `[Session activity failed: session process exited before the activity completed]` |

Identified agent sessions (`pi`, `claude`, and `codex`) retain exchange reports and useful agent wording such as `[AGENT]: …`, `[Agent interrupted]`, and `[Wait timed out …; agent active, …]`.

## Mechanism trace

1. `cmdWait` resolves every reference to `cliSession` before arming waits. That projection includes the adapter, so the CLI can distinguish known agent adapters without adding state or changing the API.
2. Single and multi-session waits call `waitSession` concurrently and buffer each report. Reports are flushed in argument order with headers for multi-wait. Exit aggregation remains failure/timeout > interruption > completion.
3. `waitSession` posts to `/v1/sessions/{id}/wait`. Predicate matches remain output-free; predicate timeout/exit diagnostics remain on stderr. HTTP/protocol/resolve errors and daemon error envelopes keep their existing stderr paths.
4. Successful `idle` and `died` conclusions and authoritative 408 timeout payloads now pass through `renderWait`. Known agent sessions continue to `renderExchangeWait`; ordinary sessions receive neutral outcome markers. Quiet mode and unread acknowledgement behavior are unchanged.
5. Local hard-deadline fallback already used neutral `[Wait timed out …; session state unknown]` language and remains unchanged.
6. Multi-wait signal handling now selects agent wording only when every observed session is an identified agent. Ordinary or mixed groups use the neutral signal notice; buffered settlement reports remain suppressed after a signal.
7. Synchronous `gmux agent prompt` still calls the agent exchange renderer and agent signal notice directly. `send --wait` is quiet and only consumes the shared exit mapping.
8. gmuxd derives process exit, closed-turn, interruption, error, and runner-death conclusions in `terminalReason` / `writeWaitConclusion`. Its generic runner-loss frame no longer hardcodes an agent diagnostic; the unchanged `runner_died` cause lets the CLI render agent or ordinary-session language from the resolved session type. Agent-action API errors remain agent-specific because those endpoints are agent-only.
9. `gmux wait` has no JSON output mode (`wait --json` is rejected), so no JSON CLI rendering contract changed. API fields and outcome vocabulary are unchanged.

## Files changed

- `cli/gmux/cmd/gmux/wait.go` — session-aware wait rendering and neutral ordinary-session outcomes.
- `cli/gmux/cmd/gmux/waitsignal.go` — neutral signal notice for ordinary/mixed waits.
- `cli/gmux/cmd/gmux/actions.go` — passes the non-agent mode through the quiet shared verdict path.
- `services/gmuxd/cmd/gmuxd/serve_central.go` — leaves generic runner loss as a typed cause for session-aware CLI rendering.
- `cli/gmux/cmd/gmux/exchange_report_test.go` — table-driven ordinary/agent outcome coverage.
- `cli/gmux/cmd/gmux/wait_multi_test.go` — single and multi ordinary-command integration coverage.
- `cli/gmux/cmd/gmux/waitsignal_test.go` — neutral command-wait signal coverage.
- `services/gmuxd/cmd/gmuxd/exchange_wait_test.go` — asserts generic runner-loss frames do not override the typed cause with fixed wording.
- `cli/gmux/cmd/gmux/help.go` and `apps/website/src/content/docs/reference/cli.md` — relevant help/reference contract updates.

## Tests

Passed:

```text
go test ./cli/gmux/cmd/gmux ./packages/adapter/... ./services/gmuxd/cmd/gmuxd -run 'Wait|ExchangeReport|RunnerLoss' -count=1
go test ./cli/gmux/... ./packages/adapter/... ./services/gmuxd/...
```

The focused tests cover agent and ordinary outcomes (snapshot/completed/interrupted/error/timeout), quiet output, single/multi buffered reporting, predicates and process exit paths already present in the suite, local deadline fallback, signals, unread acknowledgements, version-skew/protocol errors, and the daemon runner-loss message.
