import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

const gmuxdPort = process.env.VITE_DEV_PROXY_PORT || '8790'

export default defineConfig({
  plugins: [
    // reactAliasesEnabled: false — the app is Preact, but the ACP
    // conversation panel is a real-React island (@assistant-ui/react needs
    // genuine React 18 semantics: useSyncExternalStore, concurrent features,
    // context identity). Aliasing react→preact/compat (the preset default)
    // would break it. The Preact app never imports bare `react`, so turning
    // the alias off is safe; the island imports the real react/react-dom,
    // and it's lazy-loaded behind ?conv so it stays out of the main bundle.
    preact({ reactAliasesEnabled: false }),
  ],
  define: {
    // Baked into the bundle as a literal at build time. Read by
    // home.tsx to render the footer and to compare against the daemon's
    // /v1/health version: a mismatch surfaces the "reload to update"
    // prompt. Release builds MUST pass VERSION (see .goreleaser.yml's
    // before-hook); without it both backend and frontend default to
    // 'dev', which is fine for local dev but would silently break the
    // version-mismatch UX on releases.
    __GMUX_VERSION__: JSON.stringify(process.env.VERSION || 'dev'),
  },
  server: {
    allowedHosts: true,
    proxy: {
      '/v1': {
        target: `http://127.0.0.1:${gmuxdPort}`,
      },
      '/ws': {
        target: `http://127.0.0.1:${gmuxdPort}`,
        ws: true,
      },
      '/acp': {
        target: `http://127.0.0.1:${gmuxdPort}`,
        ws: true,
      },
    },
  },
})
