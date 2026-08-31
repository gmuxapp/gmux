# Mobile composable Shift report

## Identity

- Base: `origin/main` at `fac7e24e73c178b0f6695a16e9e8c9619e6993fe`
- Branch/bookmark: `feat/mobile-composable-shift`
- Implementation SHA: `bbe6f5c1595f025edd649f4e87eb7e092c6f2666`
- PR: https://github.com/gmuxapp/gmux/pull/510 (target: `main`)

## Semantics

The old dedicated `⇧tab` / BackTab action is now a button whose visible label is `shift` and accessible name is **Shift modifier**. It occupies the same `.mk-shift-tab` grid area.

Shift reuses the Ctrl/Alt mobile modifier model:

- a tap toggles the arm; arming sends no terminal input;
- armed state uses the existing accent treatment and `aria-pressed="true"`;
- the title changes between **Arm Shift** and **Shift armed for next key**;
- a supported key/text payload consumes the arm once;
- a second tap cancels it without sending;
- toolbar pointer handling preserves terminal focus and does not summon a closed keyboard;
- holding Shift has no repeat behavior and injects no literal text;
- session selection changes, terminal detach, and current-socket close/reconnect clear Shift together with Ctrl/Alt.

IME/dictation multi-character text passes through byte-for-byte and consumes Shift, preventing a latent arm. Already-uppercase keyboard input remains uppercase, avoiding double shifting. Punctuation or digits are not guessed: existing keyboard-produced text passes through and consumes Shift.

## Byte matrix

`modifier = 1 + Shift(1) + Alt(2) + Ctrl(4)`.

| Input | Arms | Output |
|---|---|---|
| Tab (`09`) | Shift | `1b 5b 5a` (`ESC [ Z`) |
| `a` | Shift | `41` (`A`) |
| `A` | Shift | `41` (`A`) |
| `c` | Shift+Ctrl | `1b 5b 39 39 3b 36 75` (`ESC [ 99 ; 6 u`) |
| `c` | Shift+Ctrl+Alt | `1b 5b 39 39 3b 38 75` (`ESC [ 99 ; 8 u`) |
| Up (`ESC [ A`) | Shift | `1b 5b 31 3b 32 41` (`ESC [ 1 ; 2 A`) |
| Left | Shift+Ctrl+Alt | `ESC [ 1 ; 8 D` |
| text from IME/dictation | Shift | unchanged UTF-8 text; Shift consumed |

Only the encoder's existing CSI letter set (`A-D`, `F`, `H`, `Z`) receives modifier injection. No speculative function/navigation/punctuation mappings were introduced. Existing toolbar arrows and word-jumps therefore compose; unsupported inputs remain unchanged.

## Pipeline trace

- xterm keyboard/soft-keyboard output enters `TerminalView.sendInput` and `applyArmedModifiers`.
- toolbar keys intentionally bypass xterm and call the raw input capability, so `MobileTerminalBar.sendKey` applies the same modifier function first.
- toolbar Shift state lives beside Ctrl/Alt in `App`, and consumption callbacks synchronously clear both state and the terminal-side refs.
- raw terminal send retains focus only when xterm already had focus; toolbar `mousedown.preventDefault()` prevents focus theft.
- paste paths continue bypassing modifier state via their captured destination capability.
- terminal WebSocket close calls the shared modifier cancellation callback before reconnect.

## Browser/layout evidence

Inspected screenshots:

- `.memory/screenshots/mobile-composable-shift-portrait-armed.png` — 390×844, armed accent visible, two-row toolbar usable.
- `.memory/screenshots/mobile-composable-shift-portrait.png` — 390×844, unarmed state.
- `.memory/screenshots/mobile-composable-shift-wide.png` — 844×390 landscape, single-row toolbar.

The existing responsive grid positions and safe-area custom properties remain unchanged. Browser assertions cover 320×568, 390×844, 700×390, and 844×390 plus synthetic 24px/28px side insets. The `shift` word remains one line and inside its painted cell.

## Tests and checks

- `pnpm --filter @gmux/web test` — 40 files, 773 tests passed.
- `pnpm --filter @gmux/web lint` — TypeScript check passed.
- `pnpm --filter @gmux/web build` — passed (existing chunk-size warning only).
- `pnpm exec playwright test --config e2e/playwright.config.ts e2e/tests/mobile-shift-tab.spec.ts` — 3 passed.
- `pnpm test:e2e` — 100 passed.
- `pnpm test` — all 7 workspace test tasks passed.
- `pnpm lint` — all 7 workspace lint tasks passed.
- `pnpm build` — all 6 workspace build tasks passed.
- `pnpm check` is currently invalid on `main`: its `moon check :build :test` script rejects the colon-prefixed identifiers. The equivalent full test/lint/build commands above passed.

Load-bearing browser coverage fails under the old dedicated BackTab implementation because it requires a `Shift modifier` toggle with `aria-pressed`, verifies arming emits zero bytes, and separately composes Tab, a printable letter, an arrow, and Shift+Ctrl+Alt+C. It also verifies repeated taps and a long hold emit nothing.

## Review follow-up

Greptile identified two valid stale-arm paths, both fixed with regression coverage:

- mobile autocorrect/replacement now separates raw synthetic erasure from the logical replacement payload; replacement text remains byte-for-byte unchanged while one-shot Shift is cancelled, so Ctrl/Alt cannot corrupt dictation and Shift cannot leak to the following key;
- Ctrl fallback controls (`@`, `[`, Backspace, and peers supported by `ctrlSequenceFor`) now report and consume armed Shift after emitting their control byte.

The focused web suite now passes 775 tests, and Playwright reproduces a selected-text `insertReplacementText`, checks raw DEL + replacement bytes, verifies Shift cancellation, and verifies the following `b` remains lowercase.

## Compatibility/accessibility

Canonical BackTab remains available as Shift then Tab. CSI modifier encoding follows the existing xterm/CSI-u behavior, so support remains application-dependent exactly where it already was. The control is exposed as a toggle button with a stable accessible name and state. No literal modifier glyph, hidden input, synthetic browser key event, or IME text is generated by activation.
