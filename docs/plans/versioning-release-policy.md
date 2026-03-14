# Versioning and release policy (draft)

## Principles

- Version user-facing artifacts, not internal implementation details.
- Keep release automation reviewable and reversible.
- Avoid publishing unnecessary npm packages.

## v1 policy

### What is versioned for users

- `gmuxd` binary versions
- `gmuxr` binary versions
- optional `gmux-api` deployment image tags

### What is not published (v1)

- `apps/gmux-web` as standalone npm app
- `apps/gmux-api` as standalone npm app package

These are deployment components, not consumer libraries.

## Contract versioning

- gmuxd REST API is versioned via URL path (`/v1/...`)
- session metadata schema includes explicit version field (`version: 1`)
- breaking contract changes require new contract version

## Monorepo internal versioning

- internal workspace packages may remain private while architecture settles
- if public SDK packages emerge later (`@gmux/protocol` etc.), adopt changesets flow then

## Future option

If/when public JS packages are introduced:

- adopt changesets for package version/changelog generation
- use trusted publishing (OIDC) for npm
- keep per-package `CHANGELOG.md`
