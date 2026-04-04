---
title: Security
description: How gmux protects your terminal sessions from unauthorized access.
---

gmux gives full interactive terminal access to your machine. This page documents the threat model, the safeguards in place, and the design decisions behind them.

## Threat model

gmux runs an HTTP server that serves a web UI, a REST API, and WebSocket connections that carry live terminal I/O. Anyone who can reach this server can:

- List all your sessions
- Read terminal output (including secrets printed to the screen)
- Type into any terminal (execute arbitrary commands)
- Launch new processes
- Kill running sessions

This is equivalent to SSH access. The server must not be reachable by anyone who shouldn't have it.

## Default posture: localhost only, always authenticated

gmuxd uses two listeners:

1. **Unix socket** (`~/.local/state/gmux/gmuxd.sock`) for local CLI-to-daemon IPC. No authentication needed; access is enforced by filesystem permissions (0600 socket, 0700 directory). This socket cannot be forwarded by VS Code, Docker, or SSH.

2. **TCP listener** (`127.0.0.1:8790` by default) for browser access. All connections require a bearer token or session cookie. There is no unauthenticated TCP access.

By default, the TCP listener binds to `127.0.0.1`:

- ✅ Accessible from the local machine only
- ❌ Not reachable from LAN, even without a firewall
- ❌ Not reachable from tailscale or other VPNs
- ❌ Not reachable from the internet

The bind address can be changed via the `GMUXD_LISTEN` environment variable for container and VPN deployments. The port can be changed in the config file.

## Remote access: tailscale only

Remote access is available via a separate, optional tailscale (tsnet) listener. When enabled, gmuxd joins your tailnet and serves on `https://hostname.your-tailnet.ts.net`. See [Remote Access](/remote-access) for setup.

This listener has three layers of protection:

### 1. Network isolation (tailscale)

The tsnet listener only accepts connections through the tailscale network. It is not reachable from the public internet or from local networks — only from devices that are members of your tailnet.

### 2. Encrypted transport (HTTPS)

The tailscale listener serves HTTPS only, using certificates issued automatically by Let's Encrypt through tailscale. There is no HTTP listener, no TLS downgrade, and no option to disable encryption.

This protects against:
- Eavesdropping on terminal I/O
- Session hijacking via network sniffing
- Man-in-the-middle attacks

### 3. Identity verification (WhoIs)

Every request (HTTP and WebSocket upgrade) is authenticated by calling tailscale's local `WhoIs` API, which returns the cryptographic identity of the connecting peer. The peer's **login name** (e.g. `user@github`) is checked against the configured allow list.

This is not a token or cookie that could be stolen — it's derived from tailscale's WireGuard key exchange. You cannot forge a WhoIs identity without possessing the peer's private key.

**Owner auto-whitelisted:** The tailscale account that owns the node is automatically added to the allow list at startup. This means the minimal config (`enabled = true`) gives access to you and only you.

**Fail-closed for others:** Additional users in the `allow` list are checked strictly. There is no "allow all" default — only the node owner and explicitly listed login names can connect.

**Denied connections are logged** with the peer's login name and device name for auditing.

## TCP authentication

All TCP connections are protected by a **256-bit bearer token** generated on first start and persisted at `~/.local/state/gmux/auth-token`. Every request must present this token via an `Authorization: Bearer <token>` header or an HTTP-only session cookie.

The token protects against unauthorized access but **not eavesdropping**. The connection is plain HTTP. On an untrusted network, an attacker who can intercept traffic can steal the token and gain full terminal access. This is acceptable when the TCP listener binds to localhost (default), when the network layer provides encryption (WireGuard tunnel, Docker bridge), or in container port-forwarding scenarios.

See [Network Listener](/develop/network-listener) for container and VPN setup.

## What we explicitly decided against

### Device name matching

The allow list matches login names only, not device names. While tailscale device names are unique within a tailnet (the control server enforces this), they can be renamed by the user. A renamed device would silently fall off the allow list.

Login names are stable identities tied to the user's authentication provider (GitHub, Google, etc.). For per-device access control, use [tailscale ACLs](https://tailscale.com/kb/1018/acls).

### Unauthenticated access

There is no unauthenticated TCP listener. All TCP connections require a bearer token. There is no "disable auth" option, even for development. The Unix socket is the only unauthenticated path, and it is protected by filesystem permissions.

## Config validation

The config file (`~/.config/gmux/host.toml`) is strictly validated at startup. gmuxd refuses to start if:

- **Unknown keys** are present — a typo like `alow` instead of `allow` would silently result in a default (empty) allow list. Unknown keys are a hard error.
- **Allow entries don't look like login names** — entries without `@` are rejected (e.g. `"not-a-login"` instead of `"user@github"`).
- **Hostname is empty** when tailscale is enabled.
- **Port is out of range** (must be 1–65535).
- **Invalid TOML syntax**.

No config file is fine — safe defaults are used (localhost only, tailscale disabled).

## Runner security

Each session is managed by a `gmux` process that exposes a Unix socket (not a TCP port). Unix sockets are protected by filesystem permissions — only processes running as the same user can connect.

`gmuxd` connects to these sockets to proxy terminal traffic. The proxy runs in the same user context.

## Recommendations

1. **Don't change the port and assume you're safe.** Security comes from the bind address and auth, not from obscurity.
2. **Audit your allow list.** Everyone on it gets full terminal access. Treat it like your SSH `authorized_keys`.
3. **Use tailscale ACLs** if you want to restrict which devices (not just which users) can reach gmux.
4. **Check the logs.** Denied connection attempts are logged with identity details. If you see unexpected denials, someone on your tailnet tried to connect.
