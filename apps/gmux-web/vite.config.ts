import { defineConfig } from 'vite'
import type { Plugin } from 'vite'
import preact from '@preact/preset-vite'
import { execSync } from 'child_process'
import { readFileSync } from 'fs'
import { createRequire } from 'module'
import { dirname, join } from 'path'

const _require = createRequire(import.meta.url)

/**
 * Vite plugin: serve and bundle @wterm/ghostty's ghostty-vt.wasm.
 * Vite does not automatically follow new URL() references inside node_modules,
 * so we replicate the old ghosttyWasm() approach for the new package path.
 */
function wtermWasm(): Plugin {
  // @wterm/ghostty exposes its WASM at <package-root>/wasm/ghostty-vt.wasm
  const pkgEntry = _require.resolve('@wterm/ghostty')
  const pkgRoot = dirname(dirname(pkgEntry)) // dist/../ → package root
  const wasmSrc = join(pkgRoot, 'wasm', 'ghostty-vt.wasm')
  return {
    name: 'wterm-wasm',
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
  plugins: [preact(), wtermWasm()],
  resolve: {
    alias: {
      // @preact/preset-vite aliases react-dom and react/jsx-runtime but not
      // react itself. Third-party packages like @pierre/trees/react call
      // `import { useState } from 'react'` directly; without this alias they
      // get the real React runtime whose internal hook state (__H) is
      // incompatible with Preact's VNode system, causing a crash loop.
      'react': 'preact/compat',
    },
    // Ensure a single copy of preact across all pre-bundled deps.
    // Without this, Vite creates a separate preact/compat pre-bundle for each
    // package that imports it (e.g. @pierre/trees/react). The two bundles each
    // carry their own copy of preact's hook state (__currentComponent/__H),
    // which are different objects, so hooks called from one copy don't find
    // the component context set by the other.
    dedupe: ['preact', 'preact/compat'],
  },
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
    // Force @pierre/trees/react into the pre-bundle pass WITH the react alias
    // already in effect. esbuild then resolves react→preact/compat and bakes a
    // single shared compat bundle, so useFileTree's hooks share the same
    // Preact singleton as the rest of the app.
    include: ['@pierre/trees > react', '@pierre/trees/react > react'],
  },
})
