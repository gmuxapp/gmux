---
title: Environment variables
description: Environment variables used and set by gmux.
tableOfContents:
  maxHeadingLevel: 3
---

## gmuxd

Variables that affect the daemon.

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUXD_LISTEN` | TCP bind address (IPv4 or IPv6). | `127.0.0.1` |
| `GMUXD_TOKEN` | Seed the auth token file on first start. | *(none)* |
| `XDG_CONFIG_HOME` | Base directory for config files. | `~/.config` |
| `XDG_STATE_HOME` | Base directory for runtime state (socket, auth token). | `~/.local/state` |
| `GMUXD_DEV_PROXY` | Proxy frontend requests to a Vite dev server (development only). | *(none)* |

### Bind address

By default gmuxd binds to `127.0.0.1` (localhost only). All TCP connections require bearer token authentication.

To bind to all interfaces (containers, VPN setups):

```bash
GMUXD_LISTEN=0.0.0.0 gmuxd run
```

The bind address is controlled exclusively by the `GMUXD_LISTEN` environment variable. It is not a config file option because it is a deployment concern, not a user preference.

### Auth token

`GMUXD_TOKEN` seeds the auth token file (`~/.local/state/gmux/auth-token`) on first start. This is a provisioning convenience for container deployments where mounting a pre-generated file is impractical.

The value must be at least 64 hex characters (`openssl rand -hex 32` produces exactly this).

**Behavior:**

| Token file | `GMUXD_TOKEN` | Result |
|------------|---------------|--------|
| missing | not set | Generate a random token, write to file |
| missing | set | Validate, write to file |
| present | not set | Use file |
| present | matches env | Use file |
| present | differs | **Refuse to start** |
| corrupted | any | **Refuse to start** |

After reading, gmuxd **unsets** `GMUXD_TOKEN` from the process environment so child shells (your terminal sessions) don't inherit it. This reduces but does not eliminate exposure: the original value may still be visible via `/proc/*/environ` or `docker inspect`. The file at `~/.local/state/gmux/auth-token` (permissions `0600`) is the primary storage and the safer long-term secret location.

For a known token in Docker Compose:

```bash
openssl rand -hex 32   # copy the output
```

```yaml
environment:
  GMUXD_TOKEN: "paste-hex-here"
  GMUXD_LISTEN: "0.0.0.0"
```

On first start, gmuxd writes the token to disk. On subsequent starts, the file already exists and the env var is verified against it.

## gmux (CLI)

Variables that affect the session runner.

| Variable | Purpose | Default |
|----------|---------|---------|
| `GMUX_ADAPTER` | Force a specific adapter instead of auto-detection. | *(auto)* |
| `GMUX_SOCKET_DIR` | Directory for per-session Unix sockets. | `/tmp/gmux-sessions` |
| `GMUX_NO_AGENT_HOOK` | Disable injecting the gmux agent extension/hook (e.g. the pi extension). An escape hatch if an agent release breaks the extension: the agent runs unmodified, and gmux loses hook-driven title/status/attribution for it. Any value other than `0`/empty disables. Read by the runner, so it covers foreground and `-d` launches; for daemon-initiated launches set it in the daemon's environment. | *(unset)* |

## Set by gmux in child processes

These are available inside every session launched by `gmux`. Use them to detect that you are running inside gmux, or to communicate back to the session runner.

| Variable | Purpose | Example |
|----------|---------|---------|
| `GMUX` | Always `1` inside a gmux session. Used for nested-session detection. | `1` |
| `GMUX_SOCKET` | Unix socket path for callbacks to the session runner. | `/tmp/gmux-sessions/sess-abc123.sock` |
| `GMUX_SESSION_ID` | Unique session identifier. | `sess-abc123` |
| `GMUX_ADAPTER` | Name of the matched adapter. | `pi`, `shell` |
| `GMUX_RUNNER_VERSION` | Version of the gmux runner hosting the session. | `0.4.0` |
| `GMUX_SESSION_SOCK` | Socket the agent extension/hook posts session + turn events to. Set only for adapters that ship a hook (pi); absent if `GMUX_NO_AGENT_HOOK` is set. | `/tmp/gmux-sessions/sess-abc123.sock` |

See [Adapter Architecture](/develop/adapter-architecture) for how to use the child-to-runner API.

## How a session's environment is sourced

When the daemon starts, resumes, or **restarts** a session (the launch buttons in the UI), it sources a fresh environment from an interactive login shell — roughly `$SHELL -l -i` run in the session's working directory — and hands that to the session. This means edits to your `~/.zshrc` / `~/.bashrc` / `~/.profile` (and per-directory hooks like `direnv`) take effect on the next launch or restart, **without** needing a `gmux daemon restart`. Clicking **Restart session** behaves like opening a fresh terminal.

The captured environment is merged onto the daemon's own environment, so session/desktop variables that your dotfiles never set — `DISPLAY`, `SSH_AUTH_SOCK`, `XDG_RUNTIME_DIR`, and similar — are preserved. (One consequence of the merge: a variable you *remove* from your dotfiles may linger until the daemon itself is restarted.)

If `$SHELL` is unset (typical for systemd- or Docker-managed daemons), this step is skipped and the session inherits the daemon's environment unchanged. The same fallback applies if the login shell fails or takes longer than 5 seconds. Sessions you start directly from a terminal (`gmux -- <cmd>`) always use that terminal's live environment.

See [ADR 0006](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0006-fresh-login-env-on-launch.md) for the full rationale.
