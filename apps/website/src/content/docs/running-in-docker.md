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
hostname = "dev"
EOF

docker compose up -d --build
docker logs dev 2>&1 | grep "login.tailscale.com"
# Visit the URL to register, then open https://dev.your-tailnet.ts.net
```

See [Remote Access](/remote-access/) for Tailscale setup details.

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

**Option 1: Environment variable (recommended for containers).** Set `GMUXD_TOKEN` in your compose file. On first start, gmuxd writes it to disk. On subsequent starts, the file already exists and the env var is verified against it.

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

The container runs `gmuxd` as its entrypoint. Inside the container, `GMUXD_LISTEN=0.0.0.0` binds to all interfaces so the host (or Tailscale) can reach the port. The entrypoint script auto-updates gmux binaries on each start.

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

Run separate containers for different projects. Each gets its own Tailscale hostname:

```toml
# data/project-a/gmux-config/host.toml
[tailscale]
enabled = true
hostname = "project-a"
```

This means switching between browser tabs per project. See [Peer Discovery & Aggregation](/planned/peer-discovery-aggregation) for the planned single-dashboard solution.
