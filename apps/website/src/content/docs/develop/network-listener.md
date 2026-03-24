---
title: Network Listener
description: Bind gmuxd to a network address for use in containers, behind a VPN, or during development.
---

:::danger[Read this page before enabling]
The network listener exposes terminal access over plain HTTP with token-based authentication. It is designed for use behind an encrypted network layer (WireGuard, Docker bridge) where you control who can reach the port. If you want to access gmux from your phone or another device, use [Tailscale remote access](/remote-access) instead.
:::

## When to use this

The network listener exists for three cases:

1. **Docker/containers** — `127.0.0.1` inside a container is unreachable from the host. You need to bind to `0.0.0.0` and map the port.
2. **Self-managed VPN** — you run your own WireGuard tunnel and want gmux to listen on the tunnel interface IP.
3. **Development** — you need to test the UI from another device on your LAN during development.

For everything else, use [Tailscale](/remote-access). It provides encrypted transport, cryptographic identity, and automatic HTTPS with zero configuration.

## Security model

The network listener is protected by a **bearer token**: a 256-bit random value generated on first use and persisted to disk. Every request must present this token via an `Authorization: Bearer <token>` header or an HTTP-only session cookie.

**What the token protects against:**
- Port scanners and casual access. Without the token, every endpoint returns 401.
- Unauthorized users on the same network. They can see the port is open but can't interact with it.

**What the token does NOT protect against:**
- **Eavesdropping on the network.** The connection is plain HTTP. Anyone who can intercept your traffic (ARP spoofing, rogue access point, compromised router) can read the token from any request and gain full access.
- **Man-in-the-middle attacks.** Same as above. HTTPS is not available on the network listener because there's no way to get a trusted TLS certificate for a private IP address.

This is why the network listener is scoped to networks you control. On a WireGuard tunnel, the tunnel itself provides encryption. On a Docker bridge, the traffic never leaves the host. On your home LAN during development, the risk is low and temporary.

**Do not leave this enabled on a laptop that connects to public WiFi.**

## Setup

### Option 1: Environment variable

```bash
GMUXD_LISTEN=10.0.0.5 gmuxd start
```

This is the preferred approach. The env var is not persistent, so it doesn't follow you to a coffee shop. For Docker, set it in the container's environment.

### Option 2: Config file

```toml
# ~/.config/gmux/config.toml
[network]
listen = "10.0.0.5"
```

The env var `GMUXD_LISTEN` takes precedence over the config file if both are set.

### Address format

The value is an IP address, optionally with a port:

| Value | Binds to |
|-------|----------|
| `10.0.0.5` | `10.0.0.5:8790` (uses the main port) |
| `10.0.0.5:9999` | `10.0.0.5:9999` |
| `0.0.0.0:8791` | All interfaces on port 8791 (for Docker) |

When using `0.0.0.0`, you must specify a different port than the main listener (default 8790), since the localhost listener already occupies that port.

### Allowed addresses

| Range | Accepted | Use case |
|-------|----------|----------|
| `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | ✅ | Home/office LAN |
| `100.64.0.0/10` | ✅ | WireGuard, Tailscale CGNAT |
| `169.254.0.0/16`, `fe80::/10` | ✅ | Link-local |
| `fd00::/8` | ✅ | IPv6 ULA (private) |
| `0.0.0.0`, `::` | ✅ | All interfaces (containers) |
| `127.0.0.1`, `::1` | ❌ | Use the default listener |
| Public IPs | ❌ | Use Tailscale |

## Authentication

### Token lifecycle

On first start with the network listener enabled, gmuxd generates a token and writes it to:

```
~/.local/state/gmux/auth-token
```

The file has `0600` permissions (owner-only read/write). The token persists across daemon restarts. To rotate it, delete the file and restart gmuxd.

### Finding the token

```bash
gmuxd auth-link
```

This prints the listen address, the token, and a ready-to-use URL:

```
Network listener: 10.0.0.5:8790
Auth token:       4c3c82...

Open this URL to authenticate:
  http://10.0.0.5:8790/auth/login?token=4c3c82...
```

### Browser access

Open the URL from `gmuxd auth-link` in a browser, or navigate to the listen address directly. Without a valid cookie, you'll see a login page where you can paste the token. After login, a session cookie is set and you won't need the token again for 90 days.

The cookie is `HttpOnly` (not accessible to JavaScript) and `SameSite=Strict` (not sent on cross-site requests).

### Programmatic access

```bash
TOKEN=$(cat ~/.local/state/gmux/auth-token)
curl -H "Authorization: Bearer $TOKEN" http://10.0.0.5:8790/v1/sessions
```

### Mobile devices

Run `gmuxd auth-link` on the host machine and scan the printed URL from your phone's camera. The URL contains the token as a query parameter. Your browser opens, the token is validated, a cookie is set, and you're redirected to the UI. The token disappears from the URL bar after the redirect.

## Docker

A typical Docker setup:

```dockerfile
ENV GMUXD_LISTEN=0.0.0.0:8791
EXPOSE 8791
```

```bash
docker run -p 8791:8791 your-gmux-image
```

The token is generated inside the container. To retrieve it:

```bash
docker exec <container> cat /root/.local/state/gmux/auth-token
```

Or read it from the container logs (it's printed at startup).

## What's blocked on the network listener

The `/v1/shutdown` endpoint is blocked entirely on the network listener, regardless of authentication. Shutdown is a localhost-only operation used by `gmuxd start --replace` to take over the port. This prevents an authenticated network user from stopping the daemon.

All other endpoints work identically to the localhost listener once authenticated.
