---
title: Remote Access
description: Access gmux from your phone, tablet, or another machine over Tailscale.
---

:::caution[Not a collaboration tool]
Remote access is for accessing **your own machine** from your other devices. It is not intended for sharing sessions with other people. Every connected user gets full, unrestricted shell access. See [Security](/security) for the full threat model.
:::

By default, gmux only listens on localhost. To access it from another device, you can enable the built-in [Tailscale](https://tailscale.com) listener. Tailscale creates an encrypted private network between your devices without opening ports or configuring firewalls.

## Setup

The fastest path: run `gmuxd remote` and follow the prompts. It handles configuration, walks you through Tailscale registration, and verifies the result. You can run it again at any time to check the status.

The steps below cover the same process in detail.

### 1. Set up Tailscale

If you haven't used Tailscale before:

1. [Create an account](https://login.tailscale.com/start) (free for personal use, up to 100 devices).
2. [Install Tailscale](https://tailscale.com/download) on the machine running gmux **and** on the device you want to connect from.
3. Sign in on both devices with the same account.

If you already use Tailscale, just make sure both devices are on the same tailnet.

### 2. Enable HTTPS and MagicDNS

In the [Tailscale admin console](https://login.tailscale.com/admin/dns):

1. Enable **MagicDNS** so devices can find each other by name.
2. Enable **HTTPS Certificates** so Tailscale can issue valid TLS certificates for `*.ts.net` hostnames.

Both are required. You can verify they're enabled by running `gmuxd remote` after setup.

### 3. Enable remote access

```bash
gmuxd remote
```

If remote access isn't configured yet, this creates `~/.config/gmux/host.toml` with Tailscale enabled. You can also edit the file manually:

```toml
[tailscale]
enabled = true
```

See [host.toml reference](/reference/host-toml/) for all fields (hostname, allow list).

:::note[Multiple machines]
Each machine on the same tailnet needs a unique hostname. Set it in `host.toml`:

```toml
[tailscale]
enabled = true
hostname = "gmux-desktop"
```

This gives you `https://gmux-desktop.your-tailnet.ts.net`.
:::

### 4. Restart and register

```bash
gmuxd restart
```

gmux registers as its own device in your tailnet, separate from the machine it runs on. On first start, gmuxd prints a login URL:

```
tsauth: tailscale needs login — visit: https://login.tailscale.com/a/...
```

Visit this URL to approve the device. After registration:

```
tsauth: node owner you@github auto-whitelisted
tsauth: connected
```

Verify with `gmuxd remote`:

```
$ gmuxd remote
  local:  http://127.0.0.1:8790
  remote: https://gmux.your-tailnet.ts.net

Remote access is active.
```

### 5. Connect

On your other device, open `https://gmux.your-tailnet.ts.net`. The connection is HTTPS with a valid certificate.

:::note
The Tailscale listener is independent from the localhost listener. Local access (`127.0.0.1:8790`) always works, so you can't lock yourself out by misconfiguring Tailscale.
:::

## Sharing access

Every connection is verified by Tailscale's cryptographic identity and checked against an allow list. Your primary account is allowed automatically. See [Security](/security) for the full verification design.

### Adding your other accounts

If you use multiple Tailscale accounts (e.g. personal and work), add them to the allow list so you can connect from either:

```toml
[tailscale]
enabled = true
allow = ["your-work-account@github"]
```

Then restart: `gmuxd restart`.

If the other account is on a **different tailnet**, you also need to share the gmux device via the [Machines](https://login.tailscale.com/admin/machines) page (⋯ → Share). The device is then accessible at `gmux.owner-tailnet.ts.net` (using the owner's tailnet name).

## Troubleshooting

Run `gmuxd remote` to diagnose most issues. It checks whether the daemon is running, whether the device is registered, and whether HTTPS and MagicDNS are enabled.

**`ERR_NAME_NOT_RESOLVED` in the browser**: the device isn't registered, or MagicDNS is disabled. Run `gmuxd remote` to check.

**gmux doesn't appear in the Tailscale dashboard**: gmux registers as its own device, not through the machine's Tailscale. Visit the login URL printed by `gmuxd run` (foreground mode shows the URL directly).

**Certificate warning**: HTTPS certificates aren't enabled in your tailnet. Enable them in your [Tailscale DNS settings](https://login.tailscale.com/admin/dns), then restart gmuxd.

**Certificate error when accessing a shared machine**: Tailscale issues certificates for the **active tailnet DNS name** only. If the owner renamed their tailnet after issuing certificates, the guest may see a mismatch. Check that the active name in [DNS settings](https://login.tailscale.com/admin/dns) matches what `gmuxd remote` shows.

**Can't reach from a specific device**: make sure Tailscale is installed, signed in, and connected on that device.
