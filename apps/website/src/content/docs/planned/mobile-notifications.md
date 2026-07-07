---
title: Mobile Notifications
description: Push notifications for mobile — current landscape and planned approach.
---

:::note[Not implemented]
This is a design sketch; nothing here has shipped.
:::

## The problem with Web Push on iOS

The standard Web Push stack (VAPID + Service Worker) works well on Android and desktop. On iOS it has two hard constraints:

1. **Home Screen install required.** Push subscriptions are only available to PWAs added to the Home Screen. A regular Safari tab cannot subscribe, and neither can Chrome, Firefox, or any other iOS browser (all run WebKit under the hood). ([WebKit blog](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/))

2. **Tapping a notification on a killed app opens Safari, not the PWA.** `clients.openWindow()` in the service worker fails silently when the app is fully closed, dropping the user into a Safari tab instead. This is a known WebKit bug, unfixed as of 2025. ([Firebase SDK issue #7698](https://github.com/firebase/firebase-js-sdk/issues/7698))

Apple introduced [Declarative Web Push](https://webkit.org/blog/16535/meet-declarative-web-push/) in Safari 18.4 (May 2025), which removes the Service Worker requirement — but not the Home Screen requirement, and it doesn't fix the killed-app tap behavior.

## Planned approach: thin native iOS wrapper

A minimal WKWebView app wrapping the existing web UI. The web UI and daemon need no changes — the native layer is purely a shell for APNs access and correct notification tap routing.

**iOS app (~150 lines of Swift)**
- `WKWebView` loading gmuxd's URL (Tailscale address or local network)
- APNs registration on launch; device token sent to gmuxd
- `UNUserNotificationCenterDelegate` — notification tap brings the app to foreground
- Safe area / keyboard inset passthrough

**gmuxd additions**
- Endpoint to store APNs device tokens per device
- APNs HTTP/2 client ([`sideshow/apns2`](https://github.com/sideshow/apns2)) + APNs auth key (`.p8`, generated once in the Apple Developer portal)
- Push triggers: session has unread output, session finishes

**Distribution:** TestFlight — one-time install, no App Store review needed for personal use.

**Prerequisites**
- Apple Developer Program ($99/year) — required for APNs and signing
- Mac with Xcode for initial build and signing; subsequent builds can use `xcodebuild` from the command line

## Why not Web Push?

| | Web Push | Native wrapper |
|---|---|---|
| Cost | Free | $99/year + Mac |
| Notification when app is killed | Broken on iOS | Works correctly |
| Tap opens | Safari (broken) | The app |
| Maintenance after setup | Low | Low — web UI changes don't touch Swift |

Web Push is fine on Android and desktop and could be added independently for those platforms at low cost. The native wrapper is the iOS-specific solution.
