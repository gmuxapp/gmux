import type { Terminal } from '@xterm/xterm'

/**
 * A Nerd Font icon code point used to probe whether the icon fallback font has
 * finished loading. U+F015 (Font Awesome "home") sits in the bundled subset
 * and inside the @font-face's unicode-range (see fonts.css), so checking it
 * tells us specifically whether the Nerd Font — not Fira Code — is available.
 */
const NERD_FONT_PROBE = '\uf015'

const NERD_FONT_FAMILY = "'Symbols Nerd Font Mono'"

/**
 * xterm's WebGL renderer rasterizes each glyph into a texture atlas the first
 * time it's drawn and caches it; it does not re-rasterize a cached glyph when
 * a web font later becomes available (this build has no `loadingdone` hook).
 *
 * Our Nerd Font icon fallback loads lazily — the @font-face carries a
 * unicode-range, so the browser only fetches the ~490 KB woff2 once an icon
 * glyph actually appears in the output. That ordering is the problem: the
 * first icon can be rasterized as tofu *before* the font bytes arrive, and the
 * tofu stays cached until something clears the atlas (a resize or font change).
 *
 * This bridges the gap. When the icon font finishes loading we clear the atlas
 * once, forcing every cached glyph to re-rasterize against the now-available
 * font. We deliberately do not *trigger* the load (that would defeat the
 * lazy/unicode-range optimization for text-only sessions) — we only react to
 * it. If the font is already loaded (another terminal pulled it in, or the user
 * has the full Nerd Font installed) there is nothing to fix and we no-op.
 *
 * Returns a disposer that detaches the listener.
 */
export function refreshAtlasWhenIconFontLoads(
  term: Pick<Terminal, 'clearTextureAtlas'>,
  fontSize: number,
  fonts: FontFaceSet = document.fonts,
): () => void {
  const spec = `${fontSize}px ${NERD_FONT_FAMILY}`

  // Already available: glyphs will rasterize correctly from the first draw,
  // so there is nothing to watch for and the disposer is a no-op.
  if (fonts.check(spec, NERD_FONT_PROBE)) {
    return () => {
      /* no listener attached */
    }
  }

  const onLoadingDone = () => {
    // `loadingdone` fires for any font (e.g. Fira Code weights); only act once
    // the icon font specifically is ready.
    if (!fonts.check(spec, NERD_FONT_PROBE)) return
    term.clearTextureAtlas()
    fonts.removeEventListener('loadingdone', onLoadingDone)
  }

  fonts.addEventListener('loadingdone', onLoadingDone)
  return () => fonts.removeEventListener('loadingdone', onLoadingDone)
}
