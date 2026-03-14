# Distribution plan v1

## Scope

Define how gmux is shipped without adding Electron complexity in v1.

## Artifacts

### Native

- `gmuxd` (primary runtime binary)
- `gmuxr` (launcher/metadata binary)

Both ship as platform-specific artifacts with checksums.

### Web/API

- `apps/gmux-web` builds static assets
- local mode: assets embedded/served by `gmuxd`
- multi-machine mode: assets served by `gmux-api` (or reverse proxy static origin)

### Optional runtime package

- `gmux-api` container image for server deployments

## Deployment modes

### Local all-in-one (sidecar boundary)

- one command starts local backend + gmuxd
- backend ↔ gmuxd still uses local REST
- UI opened in normal browser or app-mode wrapper

### Server mode

- deploy `gmux-api` behind reverse proxy + forward auth
- register satellite `gmuxd` nodes
- backend routes actions to node REST endpoints

## Desktop UX policy

- v1: browser-first, with optional app-mode launcher
- no Electron packaging/signing/update pipeline in v1
- revisit if browser shortcut conflicts become a confirmed productivity blocker

## Open items

- release tooling decision for Go binaries (goreleaser or equivalent)
- provenance/signing approach for binary downloads
- exact local command UX (`gmux up`, `gmux open`, `gmux doctor`)
