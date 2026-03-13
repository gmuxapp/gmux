# ADR-0002: gmux-run owns launch integration + metadata emission

- Status: Accepted
- Date: 2026-03-13

## Decision

`gmux-run` is the authoritative launcher integration point for connectable sessions.

Responsibilities:

- launch adapter-specific commands (pi first, generic next)
- integrate with abduco runtime
- emit session metadata and state transitions
- provide a stable adapter interface for future runtimes

`gmuxd` consumes emitted metadata and owns machine-level runtime policy.

## Why

- removes backend inference heuristics
- creates deterministic mapping from launch to session identity
- centralizes app-specific integration logic
