# @gmux/scroll-anchor

An xterm.js addon that separates two terminal scroll modes:

- **following** keeps the viewport at the newest output;
- **anchored** lets xterm preserve the user's viewport natively while output streams, and restores a content/distance anchor after `CSI 3 J` clears scrollback.

Wheel and touch movement enter anchored mode. Reaching the bottom, or calling `follow()`, returns to following mode. The addon also exposes DEC 2026 synchronized-output state and a combined `busy` fence for integrations that must defer terminal resizes through post-wipe viewport synchronization.

```ts
import { ScrollAnchorAddon } from '@gmux/scroll-anchor'

const addon = new ScrollAnchorAddon()
terminal.open(element)
terminal.loadAddon(addon)

// Session/replay boundaries and an explicit End action:
addon.follow()
```

## Current input coverage and accepted limitations

Wheel and touch scrolling are observed directly. Keyboard scrollback that does not land at the bottom is not identified as user intent in v1. Scrollbar drags are recognized structurally when they land at the bottom, but a drag to another scrollback position does not enter anchored mode.

Two timing limitations are accepted for this version:

- With `smoothScrollDuration > 0`, parsed output can land between a wheel event and its first animation frame. Parsing clears the intent latch, so that notch can be reverted; the default duration of `0` is unaffected. A follow-up could correlate latch expiry with xterm's pending scroll animation instead of `onWriteParsed`.
- During an ED3 wipe-resolution fence, scrollbar or keyboard arrival at bottom does not enter following mode because an output-driven transient also reports 0/0. Explicit wheel/touch intent at bottom is still recognized.

## xterm compatibility note

Programmatic following uses `scrollToBottom(true)` to disable smooth scrolling. The gmux xterm fork supports this optional argument, although its generated public declaration still has the upstream zero-argument signature; the addon contains a narrow cast for that mismatch. Other xterm builds ignore extra JavaScript arguments, but consumers should verify their desired smooth-scroll behavior.

## Monorepo development

The package's types point at the publishable `dist/index.d.ts`. Build it before running gmux-web TypeScript directly: `pnpm -C packages/scroll-anchor build` followed by `pnpm -C apps/gmux-web lint`. Moon tasks encode this dependency automatically.
