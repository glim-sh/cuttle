#!/usr/bin/env python3
"""Rename a font's family so metric-compatible free fonts present the Windows
family names a Windows fingerprint profile is expected to expose
(Calibri<-Carlito, Cambria<-Caladea, "Segoe UI"<-Carlito). Rewrites the name
table family/full/postscript/typographic records to the target family."""

import sys

from fontTools.ttLib import TTFont

src, target, out = sys.argv[1], sys.argv[2], sys.argv[3]
ps = target.replace(" ", "")
font = TTFont(src)
name = font["name"]
for rec in name.names:
    if rec.nameID in (1, 16):  # family / typographic family
        rec.string = target
    elif rec.nameID == 4:  # full name
        rec.string = target
    elif rec.nameID == 6:  # postscript name
        rec.string = ps
font.save(out)
print(f"{src} -> {target} ({out})")
