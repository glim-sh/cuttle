#!/usr/bin/env python3
"""Extract the METRICS of macOS system fonts into a JSON table for
rename-fonts.py --metrics. Run on a Mac; the output is checked in so the image
build (which has no Apple fonts) can reproduce macOS text measurement.

Only integers leave this script - advance widths per codepoint, hhea/OS-2
vertical metrics, upem and per-size tracking. No outlines, no font binaries: the
table is a set of measurements, which is the basis on which Liberation and
Nimbus were built. The Apple fonts themselves are never redistributed.

Usage: extract-font-metrics.py [out.json]   (default: ops/docker/macfonts/metrics.json)
"""

import json
import os
import sys

from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

SRC = "/System/Library/Fonts"

# family -> (file, ttc face index, variable-font location). Families a macOS
# fingerprint is expected to expose that have no metric-compatible free font,
# plus the ones we rename onto Liberation/DejaVu and must re-width to match.
#
# "SF Pro Text" is the system font behind CSS system-ui: SFNS.ttf at opsz 17,
# the axis minimum, which Chrome renders at every size up to 17px (it sets opsz
# to the CSS px size). HarfBuzz adds the per-size spacing from its trak table.
TARGETS = {
    "Lucida Grande": ("LucidaGrande.ttc", 0, None),
    "Geneva": ("Geneva.ttf", None, None),
    "Helvetica Neue": ("HelveticaNeue.ttc", 0, None),
    "Monaco": ("Monaco.ttf", None, None),
    "Helvetica": ("Helvetica.ttc", 0, None),
    "Menlo": ("Menlo.ttc", 0, None),
    "SF Pro Text": ("SFNS.ttf", None, {"opsz": 17, "wght": 400}),
    "SF Pro Text Bold": ("SFNS.ttf", None, {"opsz": 17, "wght": 700}),
}

ASCII = range(0x20, 0x7F)


def instance(path, location):
    return instantiateVariableFont(TTFont(path), location)


def mean_ascii_advance(font):
    cmap, hmtx = font.getBestCmap(), font["hmtx"].metrics
    return sum(hmtx[cmap[cp]][0] for cp in ASCII) / len(ASCII)


def tracking(path, font, location):
    """The normal track of trak, the one HarfBuzz applies by default. One static
    face stands in for every optical size, so above the base opsz the mean ASCII
    advance change of the real opsz instance is folded into the tracking: it
    keeps large system-ui text within a few percent instead of 10-20% wide."""
    track = font["trak"].horizData[0.0]
    axis = next(a for a in TTFont(path)["fvar"].axes if a.axisTag == "opsz")
    base = mean_ascii_advance(font)
    out = {}
    for size, value in sorted(track.items()):
        if size > location["opsz"]:
            opsz = min(size, axis.maxValue)
            value += round(mean_ascii_advance(instance(path, {**location, "opsz": opsz})) - base)
        out[f"{size:g}"] = value
    return out


out = {}
for family, (filename, index, location) in TARGETS.items():
    path = os.path.join(SRC, filename)
    if not os.path.exists(path):
        sys.exit(f"ERROR: {path} missing - run this on macOS")
    if location:
        font = instance(path, location)
    else:
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
    if location:
        out[family]["trak"] = tracking(path, font, location)
    print(f"{family:16} upem={out[family]['upem']:5} advances={len(out[family]['advances']):5}")

dest = sys.argv[1] if len(sys.argv) > 1 else "ops/docker/macfonts/metrics.json"
os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w") as fh:
    json.dump(out, fh, separators=(",", ":"), sort_keys=True)
print(f"wrote {dest} ({os.path.getsize(dest) // 1024} KB)")
