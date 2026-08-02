---
title: URLs and filters
description: "Stable session URLs, tab-scoped filters, and the query parameters that define a tab's identity."
---

Every view in the gmux dashboard has a stable URL, and a tab's scope lives in its query parameters — so a narrowed view can be bookmarked, pinned, or added to a phone's home screen.

## Routes

| URL pattern | What it shows |
|-------------|---------------|
| `/` | Home: the activity dashboard |
| `/:project` | Redirects to home (project hub pages were retired) |
| `/:project/:adapter/:slug` | A specific session's terminal |
| `/@:owner/:project/...` | A project owned by a peer host |

For example, `/gmux/pi/fix-auth-bug` links directly to a pi session in the gmux project. URLs update as you navigate, work with browser back/forward, and are bookmarkable. Session slugs remain stable across kill and resume, so a bookmarked session survives restarts.

## Tab identity: query parameters

Two query parameters define a tab's identity and are preserved across every in-app navigation. Both are omitted in the default state.

| Parameter | Effect |
|-----------|--------|
| `?filter=` | Narrow the tab to specific projects/hosts (see below) |
| `?sidebar=activity` | Sidebar in Activity view instead of Projects |

`?settings` opens the Settings modal directly.

## Filtering a tab

`?filter=` takes a comma-separated list of selectors:

| Selector | Matches |
|----------|---------|
| `gmux` | the gmux project on every host |
| `*@server` | everything on the host named `server` |
| `gmux@server` | exactly that project on that host |

Multiple selectors combine as a union: `?filter=gmux,api@server`. The filter scopes the whole tab — sidebar, home dashboard, and the waiting indicator — and every in-app link preserves it. Each selector shows as a removable chip above the sidebar list.

Because the filter lives in the URL, a narrowed tab is bookmarkable: keep one browser window per project, pin a tab to a remote host, or add a filtered view to your phone's home screen. Your own host matches by its hostname or the alias `local`.
