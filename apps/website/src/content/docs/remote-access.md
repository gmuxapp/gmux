---
title: Remote Access
description: Access gmux from your phone, tablet, or another machine over tailscale.
---

:::caution[Not a collaboration tool]
Remote access is designed for accessing **your own machine** from your other devices — your phone, tablet, or laptop. It is not intended for sharing terminal sessions with other people. Every connected user gets full, unrestricted shell access. See [Security](/security) for the full threat model.
:::

By default, gmux only listens on localhost. To access it from another device — your phone on the couch, a laptop in another room, or a tablet on the go — you can enable the built-in [tailscale](https://tailscale.com) listener.

## Quick start

```bash
gmuxd remote
```

This command walks you through setup if remote access isn't configured, or shows the connection status if it is. You can run it at any time to check the state of your remote access.

## Why tailscale?

[Tailscale](https://tailscale.com) is a zero-config VPN built on [WireGuard](https://www.wireguard.com/). It creates a private network (a "tailnet") between your devices — your desktop, phone, laptop, servers — without opening ports or configuring firewalls. Devices find each other by name (e.g. `gmux.your-tailnet.ts.net`) and all traffic is end-to-end encrypted.

gmux uses tailscale because exposing terminal access to a network demands strong guarantees:

- **Encrypted transport** — WireGuard encrypts all traffic. No one can sniff your terminal sessions, even on public Wi-Fi.
- **Cryptographic identity** — every connection is authenticated by tailscale's key exchange. Peer identity can't be spoofed.
- **No ports to open** — tailscale punches through NATs. No firewall rules, no port forwarding, no dynamic DNS.
- **Automatic HTTPS** — tailscale provides valid TLS certificates via Let's Encrypt for `*.ts.net` hostnames.

gmux adds an identity-verified **allow list** on top — see [Security](/security) for how this works at a technical level.

## Setup

### 1. Set up tailscale

If you haven't used tailscale before:

1. [Create an account](https://login.tailscale.com/start) — free for personal use with up to 100 devices.
2. [Install tailscale](https://tailscale.com/download) on the machine running gmux **and** on the device you want to connect from.
3. Sign in on both devices with the same account.

If you already use tailscale, just make sure both devices are on the same tailnet.

### 2. Enable HTTPS and MagicDNS

Both of these are required. In the [tailscale admin console](https://login.tailscale.com/admin/dns):

1. Enable **MagicDNS** — this lets devices find each other by name (e.g. `gmux.your-tailnet.ts.net`). Without it, the hostname won't resolve.
2. Enable **HTTPS Certificates** — this lets tailscale issue valid TLS certificates for `*.ts.net` hostnames. Without it, browsers will refuse to connect.

You can verify both are enabled by running `gmuxd remote` after setup.

### 3. Enable remote access

Run `gmuxd remote` on the machine where gmux is installed:

```bash
gmuxd remote
```

If remote access isn't configured yet, this creates `~/.config/gmux/host.toml` with tailscale enabled. You can also create or edit the file manually:

```toml
[tailscale]
enabled = true
```

| Field | Description |
|---|---|
| `enabled` | Start the tailscale listener. Default `false`. |
| `hostname` | The machine name on your tailnet. Default `gmux`, giving you `gmux.your-tailnet.ts.net`. |

:::note[Multiple machines]
If you run gmux on more than one machine on the same tailnet, each needs a unique `hostname` — tailscale can't register two nodes with the same name. Pick something descriptive:

```toml
# Desktop
[tailscale]
enabled = true
hostname = "gmux-desktop"

# Laptop
[tailscale]
enabled = true
hostname = "gmux-laptop"
```

This gives you `https://gmux-desktop.your-tailnet.ts.net` and `https://gmux-laptop.your-tailnet.ts.net`.
:::

### 4. Restart and register

Restart the daemon to apply the new config:

```bash
gmuxd start
```

**Important:** gmux registers as its own device in your tailnet, separate from the machine it runs on. On first start, gmuxd prints a Tailscale login URL:

```
tsauth: tailscale needs login — visit: https://login.tailscale.com/a/...
```

Visit this URL to approve the device. Once registered, you'll see it in your [Tailscale admin console](https://login.tailscale.com/admin/machines) as a separate machine named `gmux` (or whatever hostname you configured).

After registration completes:

```
tsauth: node owner you@github auto-whitelisted
tsauth: connected
```

You can verify everything is working with `gmuxd remote`:

```
$ gmuxd remote
  local:  http://127.0.0.1:8790
  remote: https://gmux.your-tailnet.ts.net

Remote access is active.
```

### 5. Connect

On your other device, open:

```
https://gmux.your-tailnet.ts.net
```

The connection is HTTPS with a valid certificate. No certificate warnings, no HTTP fallback.

## What's checked on every request

1. The connection must come through tailscale (the listener only accepts tailnet traffic).
2. gmuxd calls tailscale's `WhoIs` API to identify the connecting peer's cryptographic identity.
3. The peer's **login name** is checked against the allow list.
4. If the login name doesn't match, the request gets a `403 Forbidden` and the attempt is logged.

This check runs on every HTTP request and WebSocket upgrade — there are no session cookies or tokens that could be stolen. For the full security design, see the [Security](/security) page.

## The localhost listener is unchanged

The tailscale listener is a second, independent listener. The localhost TCP listener (`127.0.0.1:8790`) continues to work with token authentication. The Unix socket provides unauthenticated local IPC. Local access is always available — you can't lock yourself out by misconfiguring tailscale.

## Sharing with another tailnet

If you want to access gmux from a device on a different tailscale account (e.g. your work laptop is on a different tailnet than your home desktop), you can share the gmux machine to that account.

:::danger[Think twice before sharing]
Everyone with access gets full shell access to your machine, can read all terminal output (including secrets), launch processes, and kill sessions. This is equivalent to giving someone your SSH key. Only share with accounts you own or fully trust.
:::

### How to share

1. Open the [Machines](https://login.tailscale.com/admin/machines) page in your tailscale admin console.
2. Find the `gmux` device (it shows as a separate machine from your host).
3. Click the **⋯** menu → **Share** and create an invite link or send an email invite.
4. Accept the invite from your other tailscale account.

The guest accesses gmux at its full domain name: `gmux.owner-tailnet.ts.net` (using the *owner's* tailnet name, not the guest's).

For the allow list to work, add the guest account's login name:

```toml
[tailscale]
enabled = true
allow = ["your-other-account@github"]
```

Then restart gmuxd: `gmuxd start`.

### HTTPS and tailnet names

Tailscale issues TLS certificates for the **active tailnet DNS name** only. Each tailnet has two names: a hex ID (like `tailnet-fe8c.ts.net`) and an optional randomized name (like `cat-crocodile.ts.net`). Only the one you've selected as active in your [DNS settings](https://login.tailscale.com/admin/dns) gets valid certificates.

When a guest on another tailnet accesses a shared machine, they must use the FQDN with the active tailnet name. If there's a mismatch between the name the guest's client resolves and the name the certificate was issued for, the browser will show a certificate error.

If you hit this: go to your [tailscale DNS settings](https://login.tailscale.com/admin/dns) and make sure the active tailnet name matches what's shown in `gmuxd remote`. If you renamed your tailnet after issuing certificates, you may need to switch the active name back, or have the guest's client refresh (disconnect and reconnect tailscale).

## Adding other accounts (same tailnet)

If you use multiple tailscale accounts on the **same tailnet** (e.g. personal and work accounts that share a tailnet), add them to the allow list:

```toml
[tailscale]
enabled = true
allow = ["your-other-account@github"]
```

## Troubleshooting

Run `gmuxd remote` to diagnose most issues. It checks whether the daemon is running, whether the Tailscale device is registered, and whether HTTPS and MagicDNS are enabled.

**`ERR_NAME_NOT_RESOLVED` in the browser** — the device isn't registered in your tailnet, or MagicDNS is disabled. Run `gmuxd remote` to check. If the device isn't registered, restart with `gmuxd start` and look for the login URL.

**gmux doesn't appear in the Tailscale dashboard** — gmux registers as its own device, not through the machine's tailscale. You need to visit the login URL printed by `gmuxd start`. Run `gmuxd remote` to see if it's still waiting for registration.

**"tsauth: could not determine node owner"** — gmuxd couldn't identify your tailscale account. This usually means the login flow wasn't completed. Restart with `gmuxd start` and visit the login URL.

**Certificate warning** — HTTPS certificates aren't enabled in your tailnet. Enable them in your [tailscale DNS settings](https://login.tailscale.com/admin/dns), then restart gmuxd.

**Can't reach the hostname from a specific device** — Make sure tailscale is installed, signed in, and connected on that device. MagicDNS must also be enabled in the connecting client's tailscale settings (not just the admin console).
