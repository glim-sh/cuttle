#!/usr/bin/env python3
"""Rename a font's internal `name` table so a free font presents the Windows
family name a Windows fingerprint profile is expected to expose (e.g.
Arial<-Liberation Sans, Calibri<-Carlito, "Segoe UI Emoji"<-Noto Color Emoji,
"Microsoft YaHei"<-WenQuanYi Zen Hei). Font ENUMERATION (canvas measureText,
document.fonts, CSS font matching) then sees only Windows family names while the
actual glyph coverage - including color emoji and CJK - is preserved so
canvas-hash anti-bot checks still see real, coherent rendering.

Usage: rename-fonts.py <src> <target-family> <out> [--ttc-index N]

Handles .ttf/.otf and a single face of a .ttc collection (--ttc-index),
including color-emoji (CBDT/CBLC/COLR) fonts - only the name table is rewritten.
"""

import argparse

from fontTools.ttLib import TTFont

p = argparse.ArgumentParser()
p.add_argument("src")
p.add_argument("target")
p.add_argument("out")
p.add_argument("--ttc-index", type=int, default=None)
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
font.save(a.out)
print(f"{a.src} -> {target} ({a.out})")
