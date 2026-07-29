# ADR 0028: CLI output channels — stdout reports, stderr accounts of failure to report

**Status:** Accepted
**Date:** 2026-07-29
**Related:** ADR 0009 (verb-first CLI), ADR 0027 (semantic agent CLI and result-bearing wait), ADR 0030 (exchange-oriented agent reads and observational wait)
**Supersedes in part:** ADR 0027's 2026-07-28 amendment (the "Output routing" section, its `--json` envelope, and the interrupted-wait stderr notice)

## Context

The unreleased wait/prompt follow-ups split their output by verdict: a
completed turn printed the answer on stdout, every non-completed outcome
printed nothing on stdout and a status-shaped report on stderr, `--json` moved
a stable envelope to stdout regardless of outcome, and a locally interrupted
wait printed a one-line stderr notice and died from the signal.

Living with that split exposed the mistake in its premise. A wait that
observed an interrupted or failed turn has *succeeded at its job*: it produced
exactly the domain report the caller asked for. Routing that report to stderr
treats a valid answer about a bad outcome as a diagnostic, which breaks the
two ways CLIs are actually consumed: pipelines capture stdout and lose the
report precisely when it matters most, and humans reading a terminal see the
same information typeset on two interleaved channels depending on a verdict
they don't know yet. Meanwhile `--json` existed on some verbs and not others,
was shaped per-verb, and had not been released — a half-contract that would
have had to be honored forever.

This ADR fixes the channel rules once, for every gmux verb, before release.

## Decision

### 1. The exit code is the verdict; stdout is the report

A gmux invocation that can produce the domain report the caller asked for
writes that report to **stdout**, whatever the verdict. A nonzero exit with a
stdout report means "I did my job; here is the bad news" — the report is data,
not diagnostics. Exit codes keep ADR 0027 §8's global taxonomy (0 success,
1 error/timeout, 2 intentional interruption), plus 128+N for waits killed by
a local signal (below).

### 2. stderr is only for inability to produce the report

stderr carries exactly one thing: the reason gmux could **not** produce the
requested report. Usage errors, an unknown or ambiguous session reference, an
unsupported adapter, daemon/protocol failure, version skew. These exits
are nonzero with an empty stdout (except where a prior contract already put
something there, e.g. `prompt --new`'s id line — a session that exists is
reported even when what follows fails). stderr messages stay concise: one
statement of the problem, one actionable hint where one exists.

The dividing question is not "did something go wrong?" but "is this a fact
about the *domain* (the agent, its work, its timeline) or about *gmux's
ability to observe it*?" Domain facts go in the stdout report; observational
inability goes to stderr.

Runner failure splits along exactly that line. **Runner loss during an
observed activity is a domain fact** — the activity failed (ADR 0029
decision 4) — so the wait's report goes to stdout with exit 1. **Inability to
reach or arm the observation at all** — daemon unreachable, session
unresolvable, protocol failure before or while establishing the wait — is a
stderr account: gmux never got into a position to report on the domain.

### 3. Human mode is one coherent stdout document

A human-facing invocation emits **one document** on stdout: a single report
readable top to bottom, not a data channel interleaved with a commentary
channel. Markers, labels, and terminal state lines (ADR 0030's renderer) are
part of that document. Nothing of the report is duplicated to stderr.

### 4. No machine contract in this release

`--json` is removed from `wait`, `agent prompt`, and `agent logs`, and dies
with the removed `agent status` verb. It was never released; it disappears
without migration errors. This deliberately releases the human report from
purity pressure — the stdout document may say `[Agent worked for 4
iterations]` without breaking a parser, because no parser is promised
anything.

When machine output returns, its contract is fixed now: **one invocation
emits exactly one complete JSON value on stdout and no prose.** No NDJSON
side-channel mixed with prose, no partial value presented as valid output, no
per-verb ad-hoc envelopes. A machine-mode invocation that cannot produce its
value follows rule 2: empty stdout, stderr account, nonzero exit.

One known tension is recorded now rather than papered over later: two
existing contracts stream stdout *early* (`prompt --new`'s id line printed
the moment the session exists, and any future `--follow`-style output). "One
complete JSON value per invocation" must be reconciled with those shapes when
machine output is actually designed — this ADR fixes the bar, not the
reconciliation.

### 5. `--quiet` is verdict-only

`--quiet` suppresses the stdout report entirely; the exit code carries the
verdict. stderr keeps its rule-2 role. `--quiet` never changes what gmux
*does*, only what it prints.

### 6. Signals on a blocking wait: report, then die

A first SIGINT or SIGTERM delivered to a blocking `gmux wait` or synchronous
`gmux agent prompt` produces a **best-effort stdout report** of what was
observed so far, explicitly marked as a wait interruption — the agent keeps
running and gmux reached no verdict about its work — and exits **128+N**. A
second signal terminates immediately, report or no report. Under `--quiet`
the first signal produces no report either — verdict-only wins: exit 128+N,
nothing on stdout. `$?` is the contract; whether the process technically dies
signaled is not.

This replaces the prior one-line stderr notice. The rationale is rule 1
applied under pressure: at ^C the caller is *most* in need of the state
summary, and it is still domain information, so it still belongs on stdout.
128+N (not 1 or 2) preserves the prior amendment's insight that both
taxonomy codes would be lies: gmux reached no verdict about the turn.

## What this supersedes

- ADR 0027, 2026-07-28 amendment, **"Output routing"** section: non-completed
  outcomes no longer print a stderr report with empty stdout; the report is
  the stdout document. The `--json` envelope is removed. "`--quiet`
  unchanged" stands.
- ADR 0027, 2026-07-28 amendment, the **"An interrupted `wait` says what it
  is not"** bullet: the stderr notice becomes the marked stdout report of
  decision 6; the 128+N exit and the "no verdict" reasoning stand.
- ADR 0027 §11's "error or interruption: print no potentially stale/partial
  result, report the condition" — the *location and shape* of that report
  change (stdout document; ADR 0030 defines what partial content it may
  honestly include). The refusal to present stale content *as the answer*
  stands.

`send --wait` is unaffected in substance: it remains result-free and
verdict-bearing; its (empty) report was already on stdout.

## Consequences

- `gmux wait id; echo $?` composes: capture stdout, branch on the code. No
  caller has to merge two streams to learn what happened.
- Scripts that tested "non-empty stdout means completed" must test the exit
  code instead — which ADR 0027's `--new` amendment already declared the
  contract.
- Timeout and failure reports become visible to naive `$(...)` capture, which
  is the common parent-agent consumption pattern.
- The JSON reintroduction bar is explicit: a designed, versioned, whole-value
  contract or nothing.

## Alternatives considered

### Keep the verdict-split channels (answer on stdout, trouble on stderr)

Rejected. It optimizes for `output=$(gmux wait id)` treating stdout as *the
bare answer*, but ADR 0030 ends the bare-answer era anyway (the report is a
document), and the split systematically hides the most decision-relevant
reports from pipelines.

### Duplicate the report to both channels on failure

Rejected: two copies invite drift, and interactive users see it twice.

### Keep `--json` as shipped

Rejected. Freezing a per-verb, partially-thought envelope at first release is
the most expensive possible way to get a machine contract.
