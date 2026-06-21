#!/usr/bin/env bash
# Regenerate the bundled Nerd Font symbols subset.
#
# We ship a *subset* of "Symbols Nerd Font Mono" as a terminal fallback so
# icon glyphs (Powerline, file-type, git, devicons, codicons, …) render on
# any host. The full font is ~1.15 MB woff2; the subset below trims it to
# ~490 KB by dropping the largest blocks (see RANGES below).
#
# NOTE: the unicode-range descriptor in apps/gmux-web/src/fonts.css must be
# kept in sync with RANGES below, otherwise the browser may either download
# the font needlessly or fail to render glyphs we actually shipped.
#
# Users who install the *full* Nerd Font get all glyphs via the local()
# entries in fonts.css; this subset is only the no-install fallback.
#
# Requires: curl, unzip, woff2 (pip: fonttools brotli).
set -euo pipefail

DEST="$(cd "$(dirname "$0")/.." && pwd)/apps/gmux-web/src/assets"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Pinned: bumping this is an intentional, reviewable change. Using
# releases/latest would silently drift glyph code points across Nerd Fonts
# releases (e.g. the v3->v4 renames), desyncing the committed woff2 and the
# unicode-range in fonts.css.
NERD_FONTS_VERSION="v3.4.0"
URL="https://github.com/ryanoasis/nerd-fonts/releases/download/${NERD_FONTS_VERSION}/NerdFontsSymbolsOnly.zip"

# Nerd Font v3 glyph ranges (https://github.com/ryanoasis/nerd-fonts/wiki/Glyph-Sets-and-Code-Points).
# Excluded to keep the fallback small (the full font, used via local() when
# installed, still covers these):
#   - Material Design Icons  U+F0001-F1AF0  (thousands of glyphs)
#   - Font Awesome v6 block  U+ED00-EFCE    (~720 glyphs, overlaps FA legacy)
RANGES="U+23FB-23FE,U+2665,U+26A1,U+2B58,\
U+E000-E00A,U+E0A0-E0A2,U+E0A3,U+E0B0-E0B3,U+E0B4-E0C8,U+E0CA,U+E0CC-E0D7,\
U+E200-E2A9,U+E300-E3E3,U+E5FA-E6B7,U+E700-E8EF,\
U+EA60-EBEB,\
U+F000-F2FF,U+F300-F375,U+F400-F533,U+F0A0-F0DF"

cd "$TMP"
curl -sL -o sym.zip "$URL"
unzip -o sym.zip -d src >/dev/null

python3 - "$RANGES" <<'PY'
import sys
from fontTools import subset
ranges = sys.argv[1]
opts = subset.Options()
opts.flavor = 'woff2'
opts.ignore_missing_glyphs = True
opts.drop_tables += ['PfEd']
font = subset.load_font('src/SymbolsNerdFontMono-Regular.ttf', opts)
s = subset.Subsetter(options=opts)
s.populate(unicodes=subset.parse_unicodes(ranges))
s.subset(font)
subset.save_font(font, 'subset.woff2', opts)
PY

mkdir -p "$DEST"
cp subset.woff2 "$DEST/symbols-nerd-font-mono-subset.woff2"
cp src/LICENSE "$DEST/SymbolsNerdFont-LICENSE"

echo "Wrote $DEST/symbols-nerd-font-mono-subset.woff2 ($(du -h "$DEST/symbols-nerd-font-mono-subset.woff2" | cut -f1))"
