# ADR-0003: Web-first distribution, native runtime binaries, no Electron in v1

- Status: Accepted
- Date: 2026-03-13

## Context

gmux needs to support three valuable usage modes:

1. single-machine local usage with minimal setup
2. multi-machine self-hosted usage behind reverse proxy/forward auth
3. mobile browser access when desired

We also want to avoid scope creep while stabilizing core lifecycle/runtime behavior.

## Decision

### Product surface

- gmux is **web-first** in v1.
- `gmuxd` serves frontend assets in local mode.
- The same frontend is used in multi-machine mode behind `gmux-api`.

### Distribution model

- Primary distributed artifacts are native binaries:
  - `gmuxd`
  - `gmuxr`
- TypeScript apps are build/runtime components, not npm-published products.
- `apps/gmux-api` is packaged for deployment (container + node runtime option), not as an npm app package for end users.

### Desktop UX strategy

- Do **not** ship Electron in v1.
- Provide convenience launcher behavior (`gmux open`) that can use browser app-mode where available (`--app=...`) and otherwise falls back to normal browser open.
- Revisit desktop shell choice later based on real shortcut pain and ops complexity.

## Why

- keeps scope focused on core reliability and architecture
- preserves one UI codepath for local/server/mobile
- minimizes packaging and update complexity early
- keeps migration path open if a desktop shell is justified later

## Consequences

### Positive

- simpler implementation and release pipeline in v1
- no Electron-specific update/signing/packaging burden
- consistent user experience across deployment modes

### Negative

- browser-reserved shortcuts (for example Ctrl+T) may conflict in some workflows
- browser app-mode behavior differs by browser/platform

### Mitigations

- offer configurable in-app shortcuts
- document recommended local launchers per OS/browser
- evaluate a desktop shell only after usage data validates need
