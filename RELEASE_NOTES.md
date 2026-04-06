Prefix any command with `gmux` and it shows up in a live browser dashboard. Watch what every agent is doing, steer them from your phone, and aggregate sessions from every machine, container, and VM into one place.

This release stabilizes the core and adds multi-machine support.

## Multi-machine sessions

gmux now aggregates sessions across machines. Pick one gmuxd as your dashboard (the hub), and it connects outward to other instances (spokes) to merge their sessions into a single UI.

- **Tailscale auto-discovery.** Enable Tailscale on two machines and they find each other automatically. No manual peer configuration, no token exchange.
- **Devcontainer auto-discovery.** Add the [gmux devcontainer Feature](https://github.com/gmuxapp/features) to any container and gmuxd discovers it via Docker events.
- **Manual peers.** For machines not on the same tailnet, configure `[[peers]]` in `host.toml` with a URL and token.
- **Transparent proxying.** Terminal I/O, kill, resume, dismiss, and launch all forward through the hub.

## CLI lifecycle

- `gmuxd start` backgrounds, logs to `~/.local/state/gmux/gmuxd.log`, waits for health, prints PID.
- `gmuxd run` runs in the foreground for systemd, Docker, or debugging.
- `gmuxd status` shows session counts, per-peer connection state with error reasons, and the Tailscale URL.

## Terminal and configuration

- **Three config files.** `host.toml` (daemon), `settings.jsonc` (terminal/keybinds), `theme.jsonc` (colors, Windows Terminal compatible). All optional.
- **Platform-aware keybinds.** Linux: Ctrl+Alt+T/N/W. Mac: Cmd+Left/Right, Cmd+Backspace, Cmd+K.
- **Session replay** with a virtual terminal emulator. TUI state preserved exactly on reconnect.
- **OSC 52 clipboard.** Terminal apps that write to the clipboard now work in the browser.

## Security

- Unix socket for local IPC. Token auth always required on TCP.
- Tailscale identity verification via WhoIs. Strict config validation.

## Breaking changes from v0.x

- `config.toml` → `host.toml`. The `[network]` section is removed; use `GMUXD_LISTEN`.
- `keybinds.jsonc` → `"keybinds"` array inside `settings.jsonc`.
- `gmuxd` (bare) prints help. Use `gmuxd start` or `gmuxd run`.
- All TCP connections require a bearer token.

---

Full changelog: https://gmux.app/changelog/
