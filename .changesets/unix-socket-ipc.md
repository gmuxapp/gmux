---
bump: minor
---

### Security

- **Fixed unauthenticated localhost listener.** The TCP listener on `localhost:8790` previously required no authentication, which was exploitable via VS Code port forwarding, `docker -p`, and SSH tunnels. All TCP connections now require a bearer token. ([#40](https://github.com/gmuxapp/gmux/pull/40))

### Architecture

- **Unix socket for local IPC.** The `gmux` CLI and `gmuxd` now communicate via a Unix socket (`~/.local/state/gmux/gmuxd.sock`) instead of an unauthenticated TCP connection. Unix sockets cannot be forwarded by VS Code, Docker, or SSH. File permissions (0600/0700) enforce locality.
- **Single authenticated TCP listener.** The TCP listener (default `127.0.0.1:8790`) serves the web UI and API with bearer token authentication on every request. The bind address is controlled by the `GMUXD_LISTEN` env var for container use.

### CLI changes

- `gmuxd start` starts the daemon in the background, replacing any existing instance. `gmuxd restart` is an alias.
- `gmuxd run` runs the daemon in the foreground (for systemd, Docker, or debugging).
- `gmuxd` (no args) prints help.
- `gmuxd stop` replaces `gmuxd shutdown`.
- `gmuxd status` shows daemon health, listen address, and socket path.
- `gmuxd auth` replaces `gmuxd auth-link`, prints the token and a ready-to-use URL.

### Config simplification

- Removed the `[network]` config section and `listen` field. Bind address is now `GMUXD_LISTEN` env var only.
- Removed `GMUXD_PORT`, `GMUXD_ADDR`, and `GMUXD_SOCKET` environment variables.
