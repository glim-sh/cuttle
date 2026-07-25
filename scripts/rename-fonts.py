#!/usr/bin/env python3
"""Rename a font's internal `name` table so a free font presents the family name
a fingerprint profile is expected to expose (e.g. Arial<-Liberation Sans,
Calibri<-Carlito, "Segoe UI Emoji"<-Noto Color Emoji, "Microsoft YaHei"<-WenQuanYi
Zen Hei, Helvetica<-Liberation Sans, Menlo<-DejaVu Sans Mono). Font ENUMERATION
(canvas measureText, document.fonts, CSS font matching) then sees only the target
platform's family names while the actual glyph coverage - including color emoji
and CJK - is preserved so canvas-hash anti-bot checks still see real, coherent
rendering.

With --metrics, also stamp the target's METRICS: per-codepoint advance widths and
the hhea/OS-2 vertical metrics, scaled from the table's upem to this font's.
Renaming alone is not enough - detectors compare measureText widths against the
generics, and line-height comes from the vertical metrics, so a renamed font
keeping its own metrics still reads as a substitute. Only integers are copied,
never outlines, which is the basis on which Liberation and Nimbus were built.

Usage: rename-fonts.py <src> <target-family> <out> [--ttc-index N]
                       [--metrics metrics.json]

Handles .ttf/.otf and a single face of a .ttc collection (--ttc-index),
including color-emoji (CBDT/CBLC/COLR) fonts - only the name table is rewritten.
"""

import argparse
import json

from fontTools.ttLib import TTFont

p = argparse.ArgumentParser()
p.add_argument("src")
p.add_argument("target")
p.add_argument("out")
p.add_argument("--ttc-index", type=int, default=None)
p.add_argument("--metrics", help="JSON metrics table from extract-font-metrics.py")
a = p.parse_args()

target = a.target
ps = target.replace(" ", "")

kwargs = {"fontNumber": a.ttc_index} if a.ttc_index is not None else {}
font = TTFont(a.src, **kwargs)
name = font["name"]
for rec in name.names:
    if rec.nameID in (1, 16):  # family / typographic (preferred) family
        rec.string = target
    elif rec.nameID == 4:  # full name
        rec.string = target
    elif rec.nameID == 6:  # postscript name
        rec.string = ps
    elif rec.nameID == 3:  # unique id - Chrome matches this, so it must not
        rec.string = target  # keep the SOURCE family name (e.g. "Liberation Sans")

note = ""
if a.metrics:
    key = target
    with open(a.metrics) as fh:
        table = json.load(fh)
    if key not in table:
        raise SystemExit(f"{a.metrics}: no metrics for {key!r}")
    m = table[key]
    scale = font["head"].unitsPerEm / m["upem"]

    def s(v):
        return round(v * scale)

    hmtx, cmap, advances = font["hmtx"], font.getBestCmap(), m["advances"]
    stamped = 0
    for cp, gname in cmap.items():
        want = advances.get(str(cp))
        if want is not None and gname in hmtx.metrics:
            hmtx.metrics[gname] = (s(want), hmtx.metrics[gname][1])
            stamped += 1

    hhea, os2 = font["hhea"], font["OS/2"]
    hhea.ascent, hhea.descent = s(m["hhea"]["ascent"]), s(m["hhea"]["descent"])
    hhea.lineGap = s(m["hhea"]["lineGap"])
    os2.sTypoAscender = s(m["os2"]["typoAscender"])
    os2.sTypoDescender = s(m["os2"]["typoDescender"])
    os2.sTypoLineGap = s(m["os2"]["typoLineGap"])
    os2.usWinAscent, os2.usWinDescent = s(m["os2"]["winAscent"]), s(m["os2"]["winDescent"])
    note = f" [metrics {key}: {stamped} advances, upem x{scale:g}]"

font.save(a.out)
print(f"{a.src} -> {target} ({a.out}){note}")
