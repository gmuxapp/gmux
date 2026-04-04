/**
 * Frontend configuration: fetch, parse, and resolve.
 *
 * This is the entry point for consumer code. It fetches the raw config from
 * gmuxd, delegates to settings-schema.ts for validation and keybinds.ts for
 * keybind resolution, and re-exports everything consumers need.
 *
 * Config files in ~/.config/gmux/:
 *   - host.toml       — gmuxd behavior (port, network, tailscale)
 *   - settings.jsonc   — frontend preferences (terminal options, keybinds, UI prefs)
 *   - theme.jsonc      — terminal color palette (drop-in Windows Terminal theme compat)
 */

// Re-export schema types and functions that consumers need.
export {
  type SettingsConfig,
  type ThemeColors,
  type Keybind,
  DEFAULT_THEME_COLORS,
  buildTerminalOptions,
  normalizeThemeColors,
} from './settings-schema'

export {
  type ResolvedKeybind,
  IS_MAC,
  DEFAULT_KEYBINDS,
  resolveKeybinds,
  parseKeyCombo,
  keyComboToSequence,
  eventMatchesKeybind,
} from './keybinds'

// ── Fetching ──

import type { SettingsConfig, ThemeColors } from './settings-schema'

export interface FrontendConfig {
  settings: SettingsConfig | null
  themeColors: ThemeColors | null
}

/**
 * Fetch frontend config from the backend.
 * Returns nulls for missing files (the caller merges with defaults).
 */
export async function fetchFrontendConfig(): Promise<FrontendConfig> {
  try {
    const resp = await fetch('/v1/frontend-config')
    if (!resp.ok) return { settings: null, themeColors: null }
    const json = await resp.json()
    const data = json.data ?? {}
    return {
      settings: data.settings ?? null,
      themeColors: data.theme ?? null,
    }
  } catch {
    return { settings: null, themeColors: null }
  }
}
