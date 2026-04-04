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

2. **TCP listener** (`127.0.0.1:8790` by default) for browser access. Every request must present a bearer token or session cookie. There is no unauthenticated TCP access, and no option to disable auth.

By default, the TCP listener binds to `127.0.0.1`:

- ✅ Accessible from the local machine only
- ❌ Not reachable from LAN, even without a firewall
- ❌ Not reachable from Tailscale or other VPNs
- ❌ Not reachable from the internet

The bind address can be changed via `GMUXD_LISTEN` for container and VPN deployments (see [Running in Docker](/running-in-docker/)). The port can be changed in the [config file](/reference/host-toml/).

### Bearer token

The token is a 256-bit random value generated on first start and persisted at `~/.local/state/gmux/auth-token`. It can be presented via an `Authorization: Bearer <token>` header or an HTTP-only session cookie.

:::caution[Token auth requires an encrypted transport]
The TCP listener serves plain HTTP. The bearer token protects against unauthorized access, but anyone who can intercept the traffic can read the token from any request and gain full terminal access. **Token auth alone is not safe over an unencrypted network.**

This is acceptable when:
- The listener binds to **localhost** (default), where traffic never hits a network
- The network layer provides encryption (**WireGuard**, **Docker bridge**)
- A **reverse proxy** handles TLS termination (e.g. Traefik with Let's Encrypt)

For remote access without managing certificates yourself, use [Tailscale](/remote-access/). For container deployments, see [Running in Docker](/running-in-docker/) for examples with WireGuard and Traefik.
:::

## Remote access: Tailscale

Remote access is available via a separate, optional Tailscale (tsnet) listener. When enabled, gmuxd joins your tailnet and serves on `https://hostname.your-tailnet.ts.net`. See [Remote Access](/remote-access) for setup.

This listener combines three protections:

- **Network isolation.** The tsnet listener only accepts connections through the Tailscale network. It is not reachable from the public internet or local networks.
- **Encrypted transport.** HTTPS only, using certificates issued automatically by Let's Encrypt through Tailscale. No HTTP fallback, no TLS downgrade.
- **Identity verification.** Every request is authenticated by calling Tailscale's local `WhoIs` API, which returns the connecting peer's cryptographic identity. The peer's **login name** (e.g. `user@github`) is checked against the configured allow list. This identity is derived from Tailscale's WireGuard key exchange and cannot be forged without possessing the peer's private key.

The Tailscale account that owns the node is automatically added to the allow list at startup. The minimal config (`enabled = true`) gives access to you and only you. Additional login names in the `allow` list are checked strictly; there is no "allow all" default. Denied connections are logged with the peer's identity for auditing.

### Allow list design

The allow list matches **login names**, not device names. Tailscale device names are unique within a tailnet, but they can be renamed by the user. A renamed device would silently fall off the allow list. Login names are stable identities tied to the authentication provider (GitHub, Google, etc.). For per-device access control, use [Tailscale ACLs](https://tailscale.com/kb/1018/acls).

The allow list is for when you have multiple accounts on the same tailnet (e.g. a personal and a work account) or when you've shared the gmux device to another tailnet you own. It is not designed for giving other people access. See the [not a collaboration tool](/remote-access) caveat.

## Why there's no "disable auth" option

gmux faces the same security problem as [Jupyter](https://jupyter-server.readthedocs.io/en/stable/operators/security.html): both serve arbitrary code execution over HTTP in a browser. Jupyter's approach is to allow disabling auth (`NotebookApp.token = ''`), which they document as "NOT RECOMMENDED." In practice, people do it constantly, leading to [exposed notebooks running as root on public clouds](https://www.cvedetails.com/vulnerability-list/vendor_id-15653/Jupyter.html). gmux takes the stricter position: auth cannot be disabled, period. `ssh` doesn't have a "disable auth" flag, and gmux gives you the same power as SSH.

**Without auth, a misconfiguration is remote code execution.** If you set `GMUXD_LISTEN=0.0.0.0` to access gmux from another device and forget to change it back, anyone on the same network can type commands into your terminal. Take that laptop to a coffee shop, and every person on the Wi-Fi has full shell access to your machine. The token ensures that even if the listener is accidentally exposed, access still requires a secret that only you have.

**You don't lose anything.** The token is a plain file on disk at `~/.local/state/gmux/auth-token`. Any integration that needs programmatic access can read it and set the `Authorization: Bearer <token>` header. A reverse proxy like Traefik can inject the token into forwarded requests via a headers middleware, so you never interact with it directly.

For container deployments, the `GMUXD_TOKEN` environment variable can seed the token file on first start. If a token file already exists, the env var must match or gmuxd refuses to start. The variable is unset from the process after consumption so child shells don't inherit it. See [Environment variables](/reference/environment/#auth-token) for details.

The file remains the primary storage. Environment variables are inherited by child processes, visible in `/proc/*/environ`, and tend to appear in CI logs and Docker inspect output. CLI flags show up in `ps` and shell history. A file with `0600` permissions is the smallest attack surface for a long-lived secret. The env var is a provisioning convenience for containers, not a replacement for the file.

The [`examples/`](https://github.com/gmuxapp/gmux/tree/main/examples) directory has ready-to-run Docker Compose setups showing how to handle auth in common deployment scenarios: [Tailscale](/running-in-docker/#tailscale-recommended), [WireGuard](/running-in-docker/#wireguard), and [Traefik with OIDC](/running-in-docker/#reverse-proxy-with-oidc-traefik--pocketid).

**For easy access from other devices**, use [Tailscale remote access](/remote-access). It gives you HTTPS with automatic certificates and cryptographic identity verification, without any tokens to manage.

## Config validation

The config file is strictly validated at startup. Silent fallback to defaults is dangerous for security settings: a typo like `alow` instead of `allow` would silently result in an empty allow list. See [host.toml reference](/reference/host-toml/#strict-validation) for the full list of rules.

## Recommendations

1. **Don't change the port and assume you're safe.** Security comes from the bind address and auth, not from obscurity.
2. **Audit your allow list.** Everyone on it gets full terminal access. Treat it like your SSH `authorized_keys`.
3. **Use Tailscale ACLs** if you want to restrict which devices (not just which users) can reach gmux.
4. **Check the logs.** Denied connection attempts are logged with identity details.
