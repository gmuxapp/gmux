# ADR-0001: component split and sidecar-local mode

- Status: Accepted
- Date: 2026-03-13

## Decision

Use 4 components:

1. gmux UI (web)
2. gmux API/BFF (TypeScript, tRPC for frontend)
3. gmuxd (native node daemon, REST)
4. gmuxr (native wrapper + metadata authority)

Protocol boundaries:

- UI ↔ API: tRPC
- API ↔ gmuxd: REST

Single-machine mode keeps the same boundary as distributed mode: run API + gmuxd together as sidecars and communicate over local REST.

## Why

- avoids local-vs-distributed architecture drift
- keeps ownership clear (gmuxd owns session/runtime lifecycle)
- supports independent hardening and evolution
