---
title: Network Listener
description: Expose gmuxd on a network address for use in containers, behind a VPN, or during development.
---

:::danger[Read this page before enabling]
Exposing the TCP listener on a network address serves terminal access over plain HTTP with token-based authentication. It is designed for use behind an encrypted network layer (WireGuard, Docker bridge) where you control who can reach the port. If you want to access gmux from your phone or another device, use [Tailscale remote access](/remote-access) instead.
:::

## When to use this

Setting `GMUXD_LISTEN` is needed for three cases:

1. **Docker/containers** -- `127.0.0.1` inside a container is unreachable from the host. You need to bind to `0.0.0.0` and map the port.
2. **Self-managed VPN** -- you run your own WireGuard tunnel and want gmux to listen on the tunnel interface IP.
3. **Development** -- you need to test the UI from another device on your LAN during development.

For everything else, use [Tailscale](/remote-access). It provides encrypted transport, cryptographic identity, and automatic HTTPS with zero configuration.

## Security model

All TCP connections are protected by a **bearer token**: a 256-bit random value generated on first use and persisted to disk. Every request must present this token via an `Authorization: Bearer <token>` header or an HTTP-only session cookie.

**What the token protects against:**
- Port scanners and casual access. Without the token, every endpoint returns 401.
- Unauthorized users on the same network. They can see the port is open but can't interact with it.

**What the token does NOT protect against:**
- **Eavesdropping on the network.** The connection is plain HTTP. Anyone who can intercept your traffic (ARP spoofing, rogue access point, compromised router) can read the token from any request and gain full access.
- **Man-in-the-middle attacks.** Same as above. HTTPS is not available on the TCP listener because there's no way to get a trusted TLS certificate for a private IP address.

This is why exposing the listener should be scoped to networks you control. On a WireGuard tunnel, the tunnel itself provides encryption. On a Docker bridge, the traffic never leaves the host. On your home LAN during development, the risk is low and temporary.

**Do not leave this enabled on a laptop that connects to public WiFi.**

## Setup

Set the `GMUXD_LISTEN` environment variable to the desired bind address:

```bash
GMUXD_LISTEN=0.0.0.0 gmuxd start
```

The default (when unset) is `127.0.0.1`, which only accepts local connections. The port comes from the config file (default 8790).

This is an env var, not a config file option, because it is a deployment concern. The env var is not persistent, so it doesn't follow you to a coffee shop. For Docker, set it in the container's environment.

### Allowed addresses

| Range | Accepted | Use case |
|-------|----------|----------|
| `127.0.0.1`, `::1` | ✅ | Localhost (default) |
| `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | ✅ | Home/office LAN |
| `100.64.0.0/10` | ✅ | WireGuard, Tailscale CGNAT |
| `169.254.0.0/16`, `fe80::/10` | ✅ | Link-local |
| `fd00::/8` | ✅ | IPv6 ULA (private) |
| `0.0.0.0`, `::` | ✅ | All interfaces (containers) |
| Public IPs | ❌ | Use Tailscale |

## Authentication

### Token lifecycle

On first start, gmuxd generates a token and writes it to:

```
~/.local/state/gmux/auth-token
```

The file has `0600` permissions (owner-only read/write). The token persists across daemon restarts. To rotate it, delete the file and restart gmuxd.

### Finding the token

```bash
gmuxd auth
```

This prints the listen address, the token, and a ready-to-use URL:

```
Listen:     127.0.0.1:8790
Auth token: 4c3c82...

Open this URL to authenticate:
  http://127.0.0.1:8790/auth/login?token=4c3c82...
```

### Browser access

Open the URL from `gmuxd auth` in a browser, or navigate to the listen address directly. Without a valid cookie, you'll see a login page where you can paste the token. After login, a session cookie is set and you won't need the token again for 90 days.

The cookie is `HttpOnly` (not accessible to JavaScript) and `SameSite=Strict` (not sent on cross-site requests).

### Programmatic access

```bash
TOKEN=$(cat ~/.local/state/gmux/auth-token)
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8790/v1/sessions
```

### Mobile devices

Run `gmuxd auth` on the host machine and scan the printed URL from your phone's camera. The URL contains the token as a query parameter. Your browser opens, the token is validated, a cookie is set, and you're redirected to the UI. The token disappears from the URL bar after the redirect.

## Docker

A typical Docker setup:

```dockerfile
ENV GMUXD_LISTEN=0.0.0.0
EXPOSE 8790
```

```bash
docker run -p 8790:8790 your-gmux-image
```

The token is generated inside the container. To retrieve it:

```bash
docker exec <container> gmuxd auth
```

## What's blocked on the TCP listener

The `/v1/shutdown` endpoint is blocked entirely on the TCP listener, regardless of authentication. Shutdown is a local-only operation available through the Unix socket. This prevents an authenticated network user from stopping the daemon.

All other endpoints work identically once authenticated.
