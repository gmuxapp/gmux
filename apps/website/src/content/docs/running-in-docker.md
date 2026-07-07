---
title: Running in Docker
description: Run gmux in a container with remote access or LAN port mapping.
---

> For native devcontainer integration (auto-discovery, session aggregation), see [Multi-Machine Sessions](/multi-machine#devcontainer-auto-discovery).

There are several ways to access a containerized gmux, depending on your network setup. Each is available as a ready-to-run example in the [`examples/`](https://github.com/gmuxapp/gmux/tree/main/examples) directory.

## Tailscale (recommended)

The container registers as its own device on your tailnet. You get HTTPS, cryptographic identity, and access from any device on your tailnet.

**Example:** [`examples/docker-tailscale/`](https://github.com/gmuxapp/gmux/tree/main/examples/docker-tailscale)

```bash
git clone https://github.com/gmuxapp/gmux
cd gmux/examples/docker-tailscale

mkdir -p data/{workspace,gmux-config,gmux-state}
cat > data/gmux-config/host.toml << 'EOF'
[tailscale]
enabled = true
EOF

# The container's hostname becomes its Tailscale name (gmux-<hostname>).
docker compose up -d --build   # compose sets hostname: dev → joins as gmux-dev
docker logs dev 2>&1 | grep "login.tailscale.com"   # first run only; the state mount persists registration
# Visit the URL to register, then open https://gmux-dev.your-tailnet.ts.net
```

On first visit you'll see the gmux login page — being on the tailnet lets you *reach* the host, but the token is what authorizes you. Paste the container's token (`docker exec dev cat /root/.local/state/gmux/auth-token`, or run `docker exec dev gmuxd auth`).

See [Remote Access](/remote-access/) for Tailscale setup details.

### Connecting the container to your main gmux

To aggregate the container's sessions into your main dashboard:

```bash
docker exec dev gmuxd auth
# paste the printed "Connect to host" URL into Settings → Hosts → Connect to host
```

Only your machine holds the container's token — the container never gets yours — so a compromised agent inside the container can't drive your other machines ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)).

## WireGuard

If you already have a WireGuard tunnel on the host, bind gmux to the tunnel interface IP so it's only reachable through the VPN. The tunnel provides encryption; gmux provides token authentication.

**Example:** [`examples/docker-wireguard/`](https://github.com/gmuxapp/gmux/tree/main/examples/docker-wireguard)

The compose file binds to your WireGuard IP (e.g. `10.0.0.2:8790:8790`) so gmux is only reachable through the tunnel.

## Reverse proxy with OIDC (Traefik + PocketID)

For a full HTTPS setup with OIDC authentication, put Traefik in front of gmux with PocketID handling login. Traefik injects the gmux bearer token into forwarded requests via a headers middleware, so you only authenticate through your OIDC provider.

**Example:** [`examples/docker-traefik-pocketid/`](https://github.com/gmuxapp/gmux/tree/main/examples/docker-traefik-pocketid)

```
browser → Traefik (HTTPS) → PocketID (OIDC) → gmux (HTTP + token)
```

This gives you a valid Let's Encrypt certificate on your own domain, with the gmux token as a second layer you never interact with directly. The example uses PocketID but works with any OIDC provider (Authelia, Authentik, Keycloak).

## Setting the auth token

By default, gmuxd generates a random auth token on first start. For container deployments where you need a known value (reverse proxy injection, health checks, scripting), there are two options.

**Option 1: Environment variable (recommended for containers).** Set `GMUXD_TOKEN` in your compose file. On first start, gmuxd writes it to disk. On subsequent starts, the env var must match the existing file or gmuxd refuses to start. Tokens must be at least 64 hex characters (`openssl rand -hex 32`).

```bash
openssl rand -hex 32   # copy the output into your compose.yaml or .env
```

```yaml
environment:
  GMUXD_TOKEN: "paste-hex-here"
  GMUXD_LISTEN: "0.0.0.0"
```

**Option 2: Pre-generated file.** Write the token to the state directory before starting the container.

```bash
mkdir -p data/gmux-state
openssl rand -hex 32 > data/gmux-state/auth-token
chmod 600 data/gmux-state/auth-token
```

Either way, any client that needs access can set the `Authorization: Bearer <token>` header. See [Environment variables](/reference/environment/#auth-token) for the full behavior table.

## How it works

The container runs `gmuxd run` as its entrypoint. In the WireGuard and Traefik examples, `GMUXD_LISTEN=0.0.0.0` binds the TCP listener to all interfaces so the host can map the port. The Tailscale example doesn't need it — the tsnet listener is reachable over the tailnet directly, and the TCP listener stays on loopback (used only by the compose health check). The entrypoint script auto-updates gmux binaries on each start.

### Bind address

`GMUXD_LISTEN` controls which address gmuxd binds to inside the container. It's an environment variable, not a config file option, because it's a deployment concern. The default (`127.0.0.1`) only accepts local connections, which is correct for bare-metal installs but unreachable from outside a container. See [Environment variables](/reference/environment/#bind-address) for details.

### What's blocked over TCP

The `/v1/shutdown` endpoint is blocked on the TCP listener regardless of authentication. Stopping the daemon is a local-only operation available through the Unix socket. This prevents an authenticated network user from killing gmuxd.

## Customization

### Adding tools

Edit the `Dockerfile` to add packages, language runtimes, or other tools. Rebuild with:

```bash
docker compose up -d --build
```

### Persistent home

To keep installed tools and shell history across container rebuilds, mount a volume at the home directory:

```yaml
volumes:
  - ./data/home:/root
  - ./data/gmux-config:/root/.config/gmux
  - ./data/gmux-state:/root/.local/state/gmux
```

The overlay mounts for gmux config and state give the container its own Tailscale identity and hostname, separate from the host.

### Multiple projects

Run separate containers for different projects. Each joins Tailscale as `gmux-<container-hostname>`, so give each container a distinct hostname (compose `hostname:` or `docker run --hostname`):

```yaml
# docker-compose.yml
services:
  project-a:
    hostname: project-a   # → joins the tailnet as gmux-project-a
```

```toml
# data/project-a/gmux-config/host.toml
[tailscale]
enabled = true
```

The node name is owned by Tailscale after first registration (it survives container recreation as long as the state mount persists), so there is no `hostname` key in `host.toml`; alternatively, set `GMUXD_TS_HOSTNAME` in the container environment to pick the tailscale name directly. To see sessions from all containers in a single dashboard instead, set up [Multi-Machine Sessions](/multi-machine) with one gmuxd as the hub.
