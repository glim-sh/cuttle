#!/usr/bin/env python3
"""Extract the METRICS of macOS system fonts into a JSON table for
rename-fonts.py --metrics. Run on a Mac; the output is checked in so the image
build (which has no Apple fonts) can reproduce macOS text measurement.

Only integers leave this script - advance widths per codepoint, hhea/OS-2
vertical metrics, upem. No outlines, no font binaries: the table is a set of
measurements, which is the basis on which Liberation and Nimbus were built. The
Apple fonts themselves are never redistributed.

Usage: extract-font-metrics.py [out.json]   (default: ops/docker/macfonts/metrics.json)
"""

import json
import os
import sys

from fontTools.ttLib import TTFont

SRC = "/System/Library/Fonts"

# family -> (file, ttc face index). Families a macOS fingerprint is expected to
# expose that have no metric-compatible free font, plus the ones we rename onto
# Liberation/DejaVu and must re-width to match.
TARGETS = {
    "Lucida Grande": ("LucidaGrande.ttc", 0),
    "Geneva": ("Geneva.ttf", None),
    "Helvetica Neue": ("HelveticaNeue.ttc", 0),
    "Monaco": ("Monaco.ttf", None),
    "Helvetica": ("Helvetica.ttc", 0),
    "Menlo": ("Menlo.ttc", 0),
}

out = {}
for family, (filename, index) in TARGETS.items():
    path = os.path.join(SRC, filename)
    if not os.path.exists(path):
        sys.exit(f"ERROR: {path} missing - run this on macOS")
    font = TTFont(path, fontNumber=index) if index is not None else TTFont(path)
    hmtx, cmap = font["hmtx"], font.getBestCmap()
    hhea, os2 = font["hhea"], font["OS/2"]
    out[family] = {
        "upem": font["head"].unitsPerEm,
        "hhea": {"ascent": hhea.ascent, "descent": hhea.descent, "lineGap": hhea.lineGap},
        "os2": {
            "typoAscender": os2.sTypoAscender,
            "typoDescender": os2.sTypoDescender,
            "typoLineGap": os2.sTypoLineGap,
            "winAscent": os2.usWinAscent,
            "winDescent": os2.usWinDescent,
        },
        "advances": {
            str(cp): hmtx.metrics[g][0] for cp, g in sorted(cmap.items()) if g in hmtx.metrics
        },
    }
    print(f"{family:16} upem={out[family]['upem']:5} advances={len(out[family]['advances']):5}")

dest = sys.argv[1] if len(sys.argv) > 1 else "ops/docker/macfonts/metrics.json"
os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w") as fh:
    json.dump(out, fh, separators=(",", ":"), sort_keys=True)
print(f"wrote {dest} ({os.path.getsize(dest) // 1024} KB)")
