---
title: Remote Access
description: Access gmux from your phone, tablet, or another machine over Tailscale.
---

:::caution[Not a collaboration tool]
Remote access is for accessing **your own machine** from your other devices. It is not intended for sharing sessions with other people. Every connected user gets full, unrestricted shell access. See [Security](/security) for the full threat model.
:::

By default, gmux only listens on localhost. To access it from another device, you can enable the built-in [Tailscale](https://tailscale.com) listener. Tailscale creates an encrypted private network between your devices without opening ports or configuring firewalls.

## Setup

The fastest path: run `gmux remote` and follow the prompts. It handles configuration, walks you through Tailscale registration, and verifies the result. You can run it again at any time to check the status.

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

Both are required. You can verify they're enabled by running `gmux remote` after setup.

### 3. Enable and register

```bash
gmux remote
```

This walks you through the process interactively: it explains what remote access does, asks for confirmation, enables Tailscale in `~/.config/gmux/host.toml`, restarts the daemon, and waits for Tailscale to connect.

On first setup, Tailscale needs you to log in:

```
Enable remote access? [y/N] y

Enabled tailscale in /home/user/.config/gmux/host.toml
Restarting daemon...
gmuxd: running (pid 12345)
  Logs: /home/user/.local/state/gmux/gmuxd.log

Connecting to Tailscale...

To complete setup, log in to Tailscale:
  https://login.tailscale.com/a/...

After logging in, run `gmux remote` again to check the connection.
```

Visit the URL to approve the device. gmux registers as its own device in your tailnet, separate from the machine's Tailscale. After login, run `gmux remote` again:

```
$ gmux remote
Connecting to Tailscale...
  local:  http://127.0.0.1:8790
  remote: https://gmux.your-tailnet.ts.net

Remote access is active.
```

You can also edit `host.toml` manually instead:

```toml
[tailscale]
enabled = true
```

See [host.toml reference](/reference/host-toml/) for all fields (allow list).

:::note[Multiple machines]
Each machine joins the tailnet under `gmux-<its-hostname>`, derived from the OS hostname on first registration and then owned by Tailscale (it dedups names automatically). To use a specific name, set the machine's name in the Tailscale admin console or rename the host — there is no `hostname` key in `host.toml` (see [ADR 0007](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0007-host-identity-and-peer-urls.md)). A machine named `gmux-desktop` is reachable at `https://gmux-desktop.your-tailnet.ts.net`.
:::

### 4. Connect

On your other device, open `https://gmux.your-tailnet.ts.net`. The connection is HTTPS with a valid certificate. You'll be asked for the host's access token (run `gmux auth` on the host to get it, or scan its QR/connect URL) — being on the tailnet lets you *reach* the host, but the token is what authorizes you ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)).

:::note
The Tailscale listener is independent from the localhost listener. Local access (`127.0.0.1:8790`) always works, so you can't lock yourself out by misconfiguring Tailscale.
:::

## Sharing access

Every connection over the tailnet is gated twice: Tailscale's cryptographic identity is checked against an allow list (your primary account is allowed automatically), **and** the request must carry the host's bearer token. The allow list is the outer gate — who may reach the host at all — not the access decision on its own ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)). See [Security](/security) for the full verification design.

### Adding your other accounts

If you use multiple Tailscale accounts (e.g. personal and work), add them to the allow list so you can connect from either:

```toml
[tailscale]
enabled = true
allow = ["your-work-account@github"]
```

Then restart: `gmux daemon restart`.

If the other account is on a **different tailnet**, you also need to share the gmux device via the [Machines](https://login.tailscale.com/admin/machines) page (⋯ → Share). The device is then accessible at `gmux.owner-tailnet.ts.net` (using the owner's tailnet name).

## Troubleshooting

Run `gmux remote` to diagnose most issues. It checks whether the daemon is running, whether the device is registered, and whether HTTPS and MagicDNS are enabled.

**`ERR_NAME_NOT_RESOLVED` in the browser**: the device isn't registered, or MagicDNS is disabled. Run `gmux remote` to check.

**gmux doesn't appear in the Tailscale dashboard**: gmux registers as its own device, not through the machine's Tailscale. Check the daemon log for the login URL: `cat $(gmux daemon log-path) | grep tsauth`. You can also run `gmuxd run` in foreground mode to see the URL directly.

**Certificate warning**: HTTPS certificates aren't enabled in your tailnet. Enable them in your [Tailscale DNS settings](https://login.tailscale.com/admin/dns), then restart gmuxd.

**Certificate error when accessing a shared machine**: Tailscale issues certificates for the **active tailnet DNS name** only. If the owner renamed their tailnet after issuing certificates, the guest may see a mismatch. Check that the active name in [DNS settings](https://login.tailscale.com/admin/dns) matches what `gmux remote` shows.

**Can't reach from a specific device**: make sure Tailscale is installed, signed in, and connected on that device.
