---
bump: major
---

### Breaking: config file layout simplified

The three config files in `~/.config/gmux/` have been reorganized:

- **`config.toml` renamed to `host.toml`** to better reflect its purpose (daemon/host behavior, network, security).
- **`keybinds.jsonc` merged into `settings.jsonc`** as a `"keybinds"` array, alongside terminal options (fontSize, cursorStyle, scrollback, etc.) and other frontend preferences.
- **`theme.jsonc` unchanged**, but now contains only the color palette (terminal options like fontSize moved to settings.jsonc).

**Migration:** rename `config.toml` to `host.toml`. If you had a `keybinds.jsonc`, wrap its array inside `{ "keybinds": [...] }` in a new `settings.jsonc`. If you had non-color options in `theme.jsonc` (fontSize, cursorStyle, etc.), move them to `settings.jsonc`.
