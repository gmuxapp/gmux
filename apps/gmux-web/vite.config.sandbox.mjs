/**
 * Sandbox-compatible Vite config (pure ESM, no TypeScript — avoids esbuild
 * for config loading so the dev server can start inside Linux containers
 * where the pnpm store only contains darwin-arm64 esbuild binaries).
 *
 * Usage (from apps/gmux-web/):
 *   ESBUILD_BINARY_PATH=/tmp/esbuild-linux/.../esbuild VITE_MOCK=1 \
 *     node_modules/.bin/vite --config vite.config.sandbox.mjs --host 0.0.0.0
 *
 * Or use the project-root helper:  scripts/sandbox-dev.sh
 */
import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'
import { execSync } from 'child_process'
import { readFileSync, existsSync } from 'fs'
import { createRequire } from 'module'
import { dirname, join } from 'path'

const _require = createRequire(import.meta.url)

function resolveGhosttyWasm() {
  const jsEntry = _require.resolve('ghostty-web')
  const distDir = dirname(jsEntry)
  const inDist = join(distDir, 'ghostty-vt.wasm')
  if (existsSync(inDist)) return inDist
  return join(dirname(distDir), 'ghostty-vt.wasm')
}

function ghosttyWasm() {
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
// Inside a sandbox, reach the host's gmuxd via host.docker.internal.
// On macOS/Linux hosts set VITE_DEV_PROXY_HOST to override.
const gmuxdHost = process.env.VITE_DEV_PROXY_HOST ||
  (process.env.IS_SANDBOX ? 'host.docker.internal' : '127.0.0.1')

const gitHash = (() => {
  try { return execSync('git rev-parse --short HEAD').toString().trim() } catch { return 'unknown' }
})()

export default defineConfig({
  plugins: [preact(), ghosttyWasm()],
  define: {
    __GMUX_VERSION__: JSON.stringify(process.env.VERSION || `dev-${gitHash}`),
  },
  server: {
    allowedHosts: true,
    fs: {
      // The pnpm store lives outside the project root; allow it so
      // font woff2 files resolve correctly in the sandbox dev server.
      allow: [
        '/Users/james-carmody/james-agent-workspace/projects/james/gmux-file-tree',
        '/Users/james-carmody/james-agent-workspace/projects/james/gmux/node_modules',
      ],
    },
    proxy: {
      '/v1': { target: `http://${gmuxdHost}:${gmuxdPort}` },
      '/auth': { target: `http://${gmuxdHost}:${gmuxdPort}` },
      '/ws': { target: `http://${gmuxdHost}:${gmuxdPort}`, ws: true },
    },
  },
  optimizeDeps: {
    exclude: ['preact-iso'],
  },
})
