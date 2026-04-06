---
title: Planned Features
description: Features under consideration for future releases.
---

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
