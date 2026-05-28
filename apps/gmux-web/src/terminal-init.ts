import { init as initGhostty } from 'ghostty-web'

// Shared init promise — ghostty-web must be initialised once before any
// Terminal is constructed. Kicked off at module load time so the WASM is
// ready by the time the first component mounts.
export const ghosttyInitPromise: Promise<void> = initGhostty()

// Per-session prefetch cache. Avoids re-downloading (up to 20 MB) and
// re-processing (O(n²) over thousands of blocks) on every tab switch.
// Key: session ID. Value: extracted bytes to inject, or null if empty.
// Populated on first load; cleared on page reload only.
export const prefetchCache = new Map<string, Uint8Array | null>()
