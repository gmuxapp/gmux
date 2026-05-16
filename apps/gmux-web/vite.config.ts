import { defineConfig } from 'vite'
import type { Plugin } from 'vite'
import preact from '@preact/preset-vite'
import { execSync } from 'child_process'
import { readFileSync, existsSync } from 'fs'
import { createRequire } from 'module'
import { dirname, join } from 'path'

const _require = createRequire(import.meta.url)

/**
 * Resolve the ghostty-vt.wasm path from the installed package.
 * Tries the dist/ subfolder first (matches the JS entry point location),
 * then falls back to the package root.
 */
function resolveGhosttyWasm(): string {
  // _require.resolve('ghostty-web') → .../ghostty-web/dist/ghostty-web.js
  const jsEntry = _require.resolve('ghostty-web')
  const distDir = dirname(jsEntry)
  const inDist = join(distDir, 'ghostty-vt.wasm')
  if (existsSync(inDist)) return inDist
  // Fallback: package root sibling of dist/
  return join(dirname(distDir), 'ghostty-vt.wasm')
}

/**
 * Vite plugin: serve and bundle ghostty-vt.wasm.
 * In dev: adds a middleware that serves the WASM at /ghostty-vt.wasm.
 * In build: emits the WASM as a static asset in the dist root.
 */
function ghosttyWasm(): Plugin {
  const wasmSrc = resolveGhosttyWasm()
  return {
    name: 'ghostty-wasm',
    configureServer(server) {
      server.middlewares.use('/ghostty-vt.wasm', (_req, res) => {
        try {
          const data = readFileSync(wasmSrc)
          res.setHeader('Content-Type', 'application/wasm')
          res.setHeader('Cache-Control', 'max-age=3600')
          res.end(data)
        } catch {
          res.statusCode = 404
          res.end('Not found')
        }
      })
    },
    generateBundle() {
      this.emitFile({
        type: 'asset',
        fileName: 'ghostty-vt.wasm',
        source: readFileSync(wasmSrc),
      })
    },
  }
}

const gmuxdPort = process.env.VITE_DEV_PROXY_PORT || '8790'
const gmuxdHost = process.env.VITE_DEV_PROXY_HOST || '127.0.0.1'

const gitHash = (() => {
  try { return execSync('git rev-parse --short HEAD').toString().trim() } catch { return 'unknown' }
})()

const gmuxdToken = process.env.VITE_DEV_TOKEN || ''
const proxyHeaders = gmuxdToken ? { Authorization: `Bearer ${gmuxdToken}` } : {}

export default defineConfig({
  plugins: [preact(), ghosttyWasm()],
  define: {
    // Baked into the bundle as a literal at build time. Read by
    // home.tsx to render the footer and to compare against the daemon's
    // /v1/health version: a mismatch surfaces the "reload to update"
    // prompt. Release builds MUST pass VERSION (see .goreleaser.yml's
    // before-hook); without it both backend and frontend default to
    // 'dev', which is fine for local dev but would silently break the
    // version-mismatch UX on releases.
    __GMUX_VERSION__: JSON.stringify(process.env.VERSION || `dev-${gitHash}`),
  },
  server: {
    allowedHosts: true,
    proxy: {
      '/v1': {
        target: `http://${gmuxdHost}:${gmuxdPort}`,
        headers: proxyHeaders,
      },
      '/auth': {
        target: `http://${gmuxdHost}:${gmuxdPort}`,
        headers: proxyHeaders,
      },
      '/ws': {
        target: `http://${gmuxdHost}:${gmuxdPort}`,
        ws: true,
      },
    },
  },
  optimizeDeps: {
    // preact-iso is source-only (no dist build). esbuild cannot resolve its
    // dynamic import('./prerender.js') during dep pre-bundling, crashing the
    // dev server on first cold start. Excluding it lets vite serve the src
    // files directly without bundling them.
    exclude: ['preact-iso'],
  },
})
