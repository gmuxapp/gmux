# AGENTS.md

## Correctness

Prioritize verifiable, correct behavior above all else. If something has lots of race conditions or other edgecases that are difficult to test and reason about, it is likely the wrong approach. If a library does something more reliably than what we can achieve, it is worth considering.

## State discipline

Never add new state without justification. Before adding a field, ask: who owns it, who updates it, and can it be derived from existing state instead? Prefer derivation over storage. New state creates maintenance burden, sync bugs, and lifecycle complexity.

## Other rules

- Push changes and create pull requests
- Follow conventional commit standards for your PR titles.
- Use `./scripts/install.sh` to when asked to install locally
