# gmux in Docker with Tailscale

A containerized gmux setup with Tailscale remote access. Access your
container's terminal sessions from any device on your tailnet.

## Quick start

```bash
# 1. Create data directories
mkdir -p data/{workspace,gmux-config,gmux-state}

# 2. Enable Tailscale. The tailscale name is gmux-<container hostname>
#    (compose sets `hostname: dev`), so this joins as gmux-dev.
cat > data/gmux-config/host.toml << 'EOF'
[tailscale]
enabled = true
EOF

# 3. Build and start
docker compose up -d --build

# 4. Register with Tailscale (first time only)
docker logs dev 2>&1 | grep "login.tailscale.com"
# Visit the URL to approve the device

# 5. Open in browser
# https://gmux-dev.your-tailnet.ts.net
#    You'll be asked for the access token (see "Connecting" below).
```

## Connecting from your main machine

The container is a peer like any other: being on your tailnet lets you
reach it, but you authorize the connection with its token. Print the
container's connect URL and paste it into **Settings → Hosts → Connect
to host** on your main gmux:

```bash
docker exec dev gmuxd auth
# copy the "Connect to host" URL it prints (carries the token)
```

This direction — your machine holds the container's token — is the only
one that exists: the container never receives your machine's token, so a
compromise inside the container (e.g. an untrusted agent) can't drive
your other machines ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)).

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
