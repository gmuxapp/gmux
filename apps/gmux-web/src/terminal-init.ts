import { GhosttyCore } from '@wterm/ghostty'

/**
 * Load a fresh GhosttyCore instance for one terminal.
 *
 * Each WTerm MUST have its own isolated GhosttyCore — they are NOT shareable.
 * GhosttyCore.init() stores a single WASM terminal pointer (termPtr); if two
 * WTerm instances share one core, the second init() overwrites the pointer and
 * both terminals silently read/write the same WASM state (corrupt colors,
 * blended scrollback, broken SGR attributes).
 *
 * The WASM binary at /ghostty-vt.wasm is served from the browser HTTP cache
 * after the first fetch. Chrome and Firefox also cache the compiled module, so
 * the per-terminal instantiation cost is negligible beyond the first terminal.
 */
export function getGhosttyCore(): Promise<GhosttyCore> {
  return GhosttyCore.load({ scrollbackLimit: 10000, wasmPath: '/ghostty-vt.wasm' })
}

// Per-session prefetch cache. Avoids re-downloading and re-processing on
// every tab switch. Key: session ID. Value: extracted bytes or null if empty.
export const prefetchCache = new Map<string, Uint8Array | null>()
