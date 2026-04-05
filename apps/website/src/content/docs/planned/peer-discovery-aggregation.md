---
title: Peer Auto-Discovery
description: Planned automatic discovery of gmux peers on a Tailscale network.
---

> The hub protocol and devcontainer auto-discovery are shipped. See [Multi-Machine Sessions](/multi-machine) for what's available today. This page covers the remaining planned work.

## Tailscale auto-discovery

Today, peers outside of devcontainers must be configured manually in `host.toml`. Tailscale auto-discovery would make this zero-config for machines on the same tailnet.

gmuxd instances already register as Tailscale devices. Tailscale's local API (`/api/v0/status`) lists all nodes on the tailnet. gmuxd could query this periodically and look for peers:

1. List all online nodes on the tailnet.
2. Filter to nodes tagged with `tag:gmux` (via Tailscale ACL tags) or matching a configurable hostname pattern.
3. For each candidate, probe `https://<hostname>/v1/health` to confirm it's a gmuxd.
4. Subscribe to its `/v1/events` stream.

This gives zero-config discovery: install gmux on two machines on the same tailnet and they find each other.

## Canonical project URI

Sessions could gain a `project_uri` field: a normalized identifier derived from the VCS remote URL.

```
git@github.com:gmuxapp/gmux.git  -->  github.com/gmuxapp/gmux
https://github.com/gmuxapp/gmux  -->  github.com/gmuxapp/gmux
```

The runner would detect this at session startup (it already walks up from cwd to find `workspace_root`; reading `git remote get-url origin` or `jj git remote list` is one more step). The field would be included in the `/meta` response alongside `workspace_root`.

Today, cross-machine project grouping relies on the user configuring the same remote URL in both projects. Canonical project URIs would make this automatic: two sessions on different machines with the same repo remote appear under one project without configuration.

## Nested peer launch routing

Currently, launch buttons are disabled for nested peers (e.g. a devcontainer running on a remote spoke). The hub can forward a launch to a direct spoke, but not through a spoke to one of its spokes. Supporting this requires the intermediate spoke to recognize and forward the `peer` field in launch requests.
