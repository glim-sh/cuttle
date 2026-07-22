#!/usr/bin/env python3
"""Build a minimal, glyphless "sink" font whose family is `cuttle-null`.

Chromium/Skia (SkFontConfigInterface_direct) hardcodes a metric-compatible
equivalence table, so a request for a Linux family name (e.g. "Liberation
Sans") is accepted when fontconfig returns our renamed "Arial" - leaking the
Linux name. A fontconfig <match> that rewrites those names to `cuttle-null`
makes fontconfig return THIS font instead (post_config == post_match, accepted),
and because it has an empty cmap the browser finds no glyphs and falls through
to the CSS generic - so the Linux name measures identically to the base font
and is not detectable. Renders nothing; exists only to absorb those requests.
"""

import sys

from fontTools.fontBuilder import FontBuilder

out = sys.argv[1]
fb = FontBuilder(unitsPerEm=1000, isTTF=True)
fb.setupGlyphOrder([".notdef"])
fb.setupCharacterMap({})  # empty cmap: covers no codepoints -> browser falls back
fb.setupGlyf({".notdef": __import__("fontTools.pens.ttGlyphPen", fromlist=["TTGlyphPen"]).TTGlyphPen(None).glyph()})
fb.setupHorizontalMetrics({".notdef": (0, 0)})
fb.setupHorizontalHeader(ascent=800, descent=-200)
fb.setupNameTable({
    "familyName": "cuttle-null",
    "styleName": "Regular",
    "psName": "cuttle-null",
})
fb.setupOS2()
fb.setupPost()
fb.save(out)
print(f"null sink font -> {out}")
