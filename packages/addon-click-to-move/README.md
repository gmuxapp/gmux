# @gmux/addon-click-to-move

An xterm.js addon that lets you **click in the current shell/REPL prompt to
move the cursor there**, driven by [OSC 133 shell-integration](https://gitlab.freedesktop.org/Per_Bothner/specifications/blob/master/proposals/semantic-prompts.md)
prompt marks.

Unlike enabling a terminal mouse mode (DECSET 1000/1006), this addon does
**not** capture all mouse input — native text selection and scroll-wheel
behaviour are preserved. It only intercepts a plain left-click that lands
inside the current editable prompt region.

> Status: lives in-repo for now; may be extracted to its own package later.

## How it works

1. The application advertises support by emitting `OSC 133 ; A ;
   click_events=1 ST` (kitty's convention). Until it does, the addon is
   dormant.
2. The addon tracks the editable region from OSC 133 marks:
   - `A` — prompt start (and the `click_events` opt-in)
   - `B` — end of prompt / start of input (region start)
   - `C` — command executing (region becomes non-editable)
   - `D` — command finished (still non-editable until the next `A`)
   Marks are anchored to xterm markers, so they follow scrollback.
3. On a plain left-click (no modifiers, no drag) inside the region, the
   addon maps the pixel position to a grid cell and sends an SGR mouse
   report — `ESC [ < 0 ; col ; row M` / `m`, 1-based — to the application,
   which moves its own cursor.

The application is responsible for decoding the SGR report and repositioning
its cursor. The terminal grid coordinates match what the application renders
into (viewport row/col), so a standard SGR mouse parser works.

## Usage

```ts
import { Terminal } from '@xterm/xterm'
import { ClickToMoveAddon } from '@gmux/addon-click-to-move'

const term = new Terminal({ altClickMovesCursor: false })
term.open(container)

term.loadAddon(
  new ClickToMoveAddon({
    // Route reports straight to the PTY, bypassing any onData post-processing.
    sendInput: (data) => ws.send(new TextEncoder().encode(data)),
  }),
)
```

### Options

| Option            | Default                        | Description |
| ----------------- | ------------------------------ | ----------- |
| `encodeReport`    | SGR left-button press+release  | Encode a click cell into bytes for the app. |
| `sendInput`       | `terminal.input(data, true)`   | Where reports go. Use a PTY-direct sink if your host mangles `onData`. |
| `forceArmed`      | `false`                        | Arm without the `click_events` opt-in (tests / out-of-band capability). |
| `dragThresholdPx` | `3`                            | Max pointer movement still treated as a click rather than a selection. |
| `onReport`        | —                              | Diagnostics hook called on each emitted report. |

## Reconnect note (gmux)

gmux replays an emulator-reconstructed snapshot on reconnect, which does not
carry OSC 133 marks. The application should re-emit its prompt marks on the
redraw triggered by the post-reconnect SIGWINCH so the addon can re-establish
the prompt region.
