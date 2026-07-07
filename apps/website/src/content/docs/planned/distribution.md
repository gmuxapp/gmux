---
title: Distribution
description: How gmux will be shipped — binaries, packaging, and deployment modes.
---

> **Status (2.0):** largely shipped — goreleaser releases with checksums, the Homebrew tap, `install.sh`, and automatic daemon upgrade all exist. Still open: signing/provenance, `gmux doctor`, AUR/Nix packaging.

## Artifacts

### Native binaries

- **`gmuxd`** — machine daemon (discovery, proxy, embedded web UI)
- **`gmux`** — session runner (PTY, adapters, Unix socket server)

Both ship as platform-specific binaries with checksums. The web UI is compiled into `gmuxd` via `go:embed` — no separate web server needed.

### Deployment modes

**Local (default):** One command starts gmuxd + gmux on your machine. The web UI is served by gmuxd at `localhost:8790`. This is how most people will use gmux.

**Remote via tailscale:** gmuxd optionally joins your tailnet for HTTPS access from other devices. See [Remote Access](/remote-access).

## Open items

- Release tooling for Go binaries (goreleaser or equivalent)
- Provenance/signing approach for binary downloads
- CLI UX for first run (`gmux doctor`, `gmux open`)
- Homebrew / AUR / Nix packaging
