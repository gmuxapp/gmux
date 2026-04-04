# gmux in Docker with Tailscale

A containerized gmux setup with Tailscale remote access. Access your
container's terminal sessions from any device on your tailnet.

## Quick start

```bash
# 1. Create data directories
mkdir -p data/{workspace,gmux-config,gmux-state}

# 2. Configure Tailscale (pick a unique hostname)
cat > data/gmux-config/host.toml << 'EOF'
[tailscale]
enabled = true
hostname = "dev"
EOF

# 3. Build and start
docker compose up -d --build

# 4. Register with Tailscale (first time only)
docker logs dev 2>&1 | grep "login.tailscale.com"
# Visit the URL to approve the device

# 5. Open in browser
# https://dev.your-tailnet.ts.net
```

## What's included

- **Auto-update**: gmux binaries are updated on each container start
- **Tailscale remote access**: the container registers as its own device
- **Persistent workspace**: repos survive container rebuilds
- **Health check**: compose monitors that gmuxd is responding

## Customization

Edit the `Dockerfile` to add your tools (language runtimes, editors, CLIs).
Edit `compose.yaml` to change the shell, add volumes, or adjust the health check.

See the [Running in Docker](https://gmux.app/running-in-docker/) guide for
more options (shared home, multiple projects, pre-generated auth tokens).
