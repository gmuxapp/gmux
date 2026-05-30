import { GhosttyCore } from '@wterm/ghostty'

// Shared init promise — GhosttyCore.load() must only be called once; the
// singleton is safe because each WTerm.init() allocates separate WASM state.
let _corePromise: Promise<GhosttyCore> | null = null

export function getGhosttyCore(): Promise<GhosttyCore> {
  if (!_corePromise) {
    _corePromise = GhosttyCore.load({ scrollbackLimit: 10000, wasmPath: '/ghostty-vt.wasm' })
  }
  return _corePromise
}

// Per-session prefetch cache. Avoids re-downloading and re-processing on
// every tab switch. Key: session ID. Value: extracted bytes or null if empty.
export const prefetchCache = new Map<string, Uint8Array | null>()
