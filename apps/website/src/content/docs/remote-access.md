---
title: Remote Access
description: Access gmux from your phone, tablet, or another machine over tailscale.
---

:::danger[Full shell access]
Anyone you grant remote access to can run arbitrary commands on your machine, read all terminal output (including secrets), and interact with every session. Treat the allow list like your SSH `authorized_keys` — only add people you fully trust.
:::

By default, gmux only listens on localhost. To access it from another device — your phone on the couch, a laptop in another room, or a tablet on the go — you can enable the built-in tailscale listener.

## Why tailscale?

gmux gives you full terminal access to your machine. Exposing that to a network requires strong security guarantees:

- **Encrypted transport** — tailscale uses WireGuard, so all traffic is end-to-end encrypted. No one on the network can sniff your terminal sessions.
- **Cryptographic identity** — every connection is authenticated by tailscale's key exchange. You can't spoof a peer identity.
- **No ports to open** — tailscale punches through NATs. You don't need to open firewall ports or set up port forwarding.

gmux adds an **allow list** on top: only tailscale users you explicitly name can connect. Everyone else gets a 403.

## Setup

### 1. Install tailscale

If you haven't already, [install tailscale](https://tailscale.com/download) on both the machine running gmux and the device you want to connect from.

### 2. Configure gmux

Create or edit `~/.config/gmux/config.toml`:

```toml
[tailscale]
enabled = true
hostname = "gmux"
```

That's it. Your own tailscale account is automatically whitelisted — gmuxd detects the node owner at startup.

| Field | Description |
|---|---|
| `enabled` | Start the tailscale listener. Default `false`. |
| `hostname` | The machine name on your tailnet. This becomes `gmux.your-tailnet.ts.net`. |
| `allow` | Additional tailscale login names that can connect. Your own account is always included automatically. |

### 3. Restart gmuxd

```bash
# If gmuxd is running, kill it — gmuxr will auto-start it next time
pkill gmuxd
gmuxr pi  # or any command — gmuxd starts automatically
```

Look for the log line:

```
tsauth: node owner you@github auto-whitelisted
tsauth: listening on https://gmux (allowed: [you@github])
```

### 4. Connect

On your other device, open:

```
https://gmux.your-tailnet.ts.net
```

The connection is HTTPS with a valid certificate (issued automatically by tailscale via Let's Encrypt). No certificate warnings, no HTTP fallback.

## What's checked on every request

1. The connection must come through tailscale (the listener only accepts tailnet traffic).
2. gmuxd calls tailscale's `WhoIs` API to identify the connecting peer.
3. The peer's **login name** is checked against the `allow` list.
4. If the login name doesn't match, the request gets a `403 Forbidden` and the attempt is logged.

This check runs on every HTTP request and WebSocket upgrade — there are no session cookies or tokens that could be stolen.

## The localhost listener is unchanged

The tailscale listener is a second, independent listener. The localhost listener (`127.0.0.1:8790`) continues to work exactly as before, with no authentication. Local access is always available — you can't lock yourself out by misconfiguring the allow list.

## Multiple users

Your own account is always included. To also grant access to a colleague:

```toml
[tailscale]
enabled = true
hostname = "gmux"
allow = ["colleague@github"]
```

Everyone on the allow list (plus you) gets the same full access. There are no permission levels — if someone can connect, they can see and interact with all sessions. Only add people you trust with terminal access to your machine.

## Troubleshooting

**"tsauth: could not determine node owner"** — gmuxd couldn't identify your tailscale account. Make sure tailscale is logged in (`tailscale status`) and try again.

**Can't reach the hostname** — Make sure both devices are on the same tailnet and that MagicDNS is enabled in your tailscale admin console.

**Certificate warning** — This shouldn't happen with tailscale's automatic HTTPS. If it does, check that HTTPS certificates are enabled in your [tailscale DNS settings](https://login.tailscale.com/admin/dns).
