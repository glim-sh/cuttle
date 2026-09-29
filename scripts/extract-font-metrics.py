#!/usr/bin/env python3
"""Extract the METRICS of macOS or Windows system fonts into a JSON table for
rename-fonts.py --metrics. Run on the platform itself, or point --src at a copy
of its font directory; the output is checked in so the image build (which has
no Apple or Microsoft fonts) can reproduce that platform's text measurement.

Only integers leave this script - advance widths per codepoint, hhea/OS-2
vertical metrics, upem, per-size tracking and, for Windows, kerning as codepoint
pairs. No outlines, no font binaries: the table is a set of measurements, which
is the basis on which Liberation and Nimbus were built. The Apple and Microsoft
fonts are never redistributed.

Usage: extract-font-metrics.py {macos,windows} [--src DIR] [--out FILE]
                               [--targets DIR]
  (defaults: the platform's font directory, ops/docker/{mac,win}fonts/metrics.json)
  --targets (windows, required): a directory holding the free faces the table is
  stamped onto (selawk*.ttf from the Selawik release zip, Carlito-*.ttf), so the
  kerning is kept to the codepoints each of them has.
"""

import argparse
import json
import os
import sys

from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

# family -> (file, ttc face index, variable-font location). Families a macOS
# fingerprint is expected to expose that have no metric-compatible free font,
# plus the ones we rename onto Liberation/DejaVu and must re-width to match.
# A .ttc face index is the face Chrome picks for that family's regular weight.
#
# "SF Pro Text" is the system font behind CSS system-ui: SFNS.ttf at opsz 17,
# the axis minimum, which Chrome renders at every size up to 17px (it sets opsz
# to the CSS px size). HarfBuzz adds the per-size spacing from its trak table.
MACOS = {
    "Lucida Grande": ("LucidaGrande.ttc", 0, None),
    "Geneva": ("Geneva.ttf", None, None),
    "Helvetica Neue": ("HelveticaNeue.ttc", 0, None),
    "Monaco": ("Monaco.ttf", None, None),
    "Helvetica": ("Helvetica.ttc", 0, None),
    "Menlo": ("Menlo.ttc", 0, None),
    "SF Pro Text": ("SFNS.ttf", None, {"opsz": 17, "wght": 400}),
    "SF Pro Text Bold": ("SFNS.ttf", None, {"opsz": 17, "wght": 700}),
    "Verdana": ("Supplemental/Verdana.ttf", None, None),
    "Verdana Bold": ("Supplemental/Verdana Bold.ttf", None, None),
    "Tahoma": ("Supplemental/Tahoma.ttf", None, None),
    "Tahoma Bold": ("Supplemental/Tahoma Bold.ttf", None, None),
    "Trebuchet MS": ("Supplemental/Trebuchet MS.ttf", None, None),
    "Trebuchet MS Bold": ("Supplemental/Trebuchet MS Bold.ttf", None, None),
    "Georgia": ("Supplemental/Georgia.ttf", None, None),
    "Georgia Bold": ("Supplemental/Georgia Bold.ttf", None, None),
    "Avenir": ("Avenir.ttc", 0, None),
    "Avenir Next": ("Avenir Next.ttc", 7, None),
    "Futura": ("Supplemental/Futura.ttc", 0, None),
}

# "Segoe UI" is the Windows system font behind CSS system-ui. It carries no trak
# or STAT table, so its advances alone reproduce real Chrome's widths.
WINDOWS = {
    "Segoe UI": ("segoeui.ttf", None, None),
    "Segoe UI Bold": ("segoeuib.ttf", None, None),
    "Segoe UI Italic": ("segoeuii.ttf", None, None),
    "Segoe UI Bold Italic": ("segoeuiz.ttf", None, None),
    "Segoe UI Symbol": ("seguisym.ttf", None, None),
    "Verdana": ("verdana.ttf", None, None),
    "Verdana Bold": ("verdanab.ttf", None, None),
    "Tahoma": ("tahoma.ttf", None, None),
    "Tahoma Bold": ("tahomabd.ttf", None, None),
    "Trebuchet MS": ("trebuc.ttf", None, None),
    "Trebuchet MS Bold": ("trebucbd.ttf", None, None),
    "Georgia": ("georgia.ttf", None, None),
    "Georgia Bold": ("georgiab.ttf", None, None),
    "Consolas": ("consola.ttf", None, None),
    "Consolas Bold": ("consolab.ttf", None, None),
    "Lucida Console": ("lucon.ttf", None, None),
    "Impact": ("impact.ttf", None, None),
    "Comic Sans MS": ("comic.ttf", None, None),
    "Comic Sans MS Bold": ("comicbd.ttf", None, None),
}

# Segoe UI is kerned over the codepoints of the free face it is stamped onto
# (ops/docker/Dockerfile); every other family over KERN_RANGE, which covers the
# Latin text a width probe uses. A family without kerning still gets an empty
# table, so the stand-in's own kerning is removed.
KERN_TARGETS = {
    "Segoe UI": "selawk.ttf",
    "Segoe UI Bold": "selawkb.ttf",
    "Segoe UI Italic": "Carlito-Italic.ttf",
    "Segoe UI Bold Italic": "Carlito-BoldItalic.ttf",
}

PLATFORMS = {
    "macos": (MACOS, "/System/Library/Fonts", "ops/docker/macfonts/metrics.json"),
    "windows": (WINDOWS, "C:/Windows/Fonts", "ops/docker/winfonts/metrics.json"),
}

ASCII = range(0x20, 0x7F)
GDEF_MARK_CLASS = 3
KERN_HORIZONTAL, KERN_CROSS_STREAM = 0x1, 0x4  # legacy kern subtable coverage bits
KERN_RANGE = range(0x20, 0x180)


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


def kern_lookups(font):
    """The GPOS lookups HarfBuzz applies for the default kern feature on Latin
    text (script latn, else DFLT; default language), in lookup-index order."""
    if "GPOS" not in font:
        return []
    gpos = font["GPOS"].table
    scripts = {r.ScriptTag: r.Script for r in gpos.ScriptList.ScriptRecord}
    script = scripts.get("latn") or scripts.get("DFLT")
    langsys = script and script.DefaultLangSys
    if not langsys:
        return []
    records = gpos.FeatureList.FeatureRecord
    indices = {
        i for f in langsys.FeatureIndex if records[f].FeatureTag == "kern"
        for i in records[f].Feature.LookupListIndex
    }
    return [gpos.LookupList.Lookup[i] for i in sorted(indices)]


def pair_subtable(sub):
    """A PairPos subtable as g1 -> (g2 -> x-advance adjustment | None): None
    when the subtable does not apply to the pair, so the next one is tried."""
    if sub.ValueFormat2:
        sys.exit("ERROR: PairPos with a second-glyph value is not supported")
    cov = sub.Coverage.glyphs
    if sub.Format == 1:
        table = {
            g1: {r.SecondGlyph: getattr(r.Value1, "XAdvance", 0) or 0 for r in ps.PairValueRecord}
            for g1, ps in zip(cov, sub.PairSet)
        }
        return lambda g1: table[g1].get if g1 in table else None
    # Format 2 applies to every second glyph once the first is covered (class 0
    # included), which is what shadows later subtables.
    covered, cd1, cd2 = set(cov), sub.ClassDef1.classDefs, sub.ClassDef2.classDefs
    rows = [[getattr(r.Value1, "XAdvance", 0) or 0 for r in c1.Class2Record] for c1 in sub.Class1Record]
    return lambda g1: (lambda g2, row=rows[cd1.get(g1, 0)]: row[cd2.get(g2, 0)]) if g1 in covered else None


def kerning(font, cps):
    """Kerning as {left: {right: value}} codepoint pairs in font units,
    over the given codepoints, resolved as HarfBuzz does for the default kern
    feature: GPOS kern lookups summed, the first applying subtable winning within
    each; the legacy kern table only when GPOS has no kern feature. Marks are
    skipped, as every kern lookup here ignores them."""
    cmap = font.getBestCmap()
    gdef = font["GDEF"].table.GlyphClassDef if "GDEF" in font else None
    marks = {g for g, c in gdef.classDefs.items() if c == GDEF_MARK_CLASS} if gdef else set()
    glyphs = [(cp, cmap[cp]) for cp in sorted(cps) if cp in cmap and cmap[cp] not in marks]

    lookups = []
    for lookup in kern_lookups(font):
        subs = [s.ExtSubTable if lookup.LookupType == 9 else s for s in lookup.SubTable]
        lookups.append([pair_subtable(s) for s in subs if s.LookupType == 2])
    if not lookups and "kern" in font:
        legacy = {}
        for st in font["kern"].kernTables:
            horizontal = st.coverage & KERN_HORIZONTAL and not st.coverage & KERN_CROSS_STREAM
            if st.format == 0 and horizontal:
                for (l, r), v in st.kernTable.items():
                    legacy.setdefault(l, {}).setdefault(r, 0)
                    legacy[l][r] += v
        lookups = [[lambda g1: legacy[g1].get if g1 in legacy else None]]

    out = {}
    for cp1, g1 in glyphs:
        rows = [[get for sub in lk if (get := sub(g1))] for lk in lookups]
        if not any(rows):
            continue
        for cp2, g2 in glyphs:
            total = 0
            for row in rows:
                total += next((v for get in row if (v := get(g2)) is not None), 0)
            if total:
                out.setdefault(str(cp1), {})[str(cp2)] = total
    return out


ap = argparse.ArgumentParser()
ap.add_argument("platform", choices=PLATFORMS)
ap.add_argument("--src", help="font directory (default: the platform's own)")
ap.add_argument("--out", help="metrics table to write")
ap.add_argument("--targets", help="directory of the free faces to kern (windows)")
a = ap.parse_args()
targets, src, dest = PLATFORMS[a.platform]
src, dest = a.src or src, a.out or dest
if a.platform == "windows" and not a.targets:
    sys.exit("ERROR: windows needs --targets DIR (the Selawik and Carlito faces)")

out = {}
for family, (filename, index, location) in targets.items():
    path = os.path.join(src, filename)
    if not os.path.exists(path):
        sys.exit(f"ERROR: {path} missing - run this on {a.platform} or pass --src")
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
            # CoreText sizes a font from hhea whatever this bit says; Windows
            # and FreeType switch to the typo metrics when it is set.
            "useTypoMetrics": a.platform == "windows" and bool(os2.fsSelection & 0x80),
        },
        "advances": {
            str(cp): hmtx.metrics[g][0] for cp, g in sorted(cmap.items()) if g in hmtx.metrics
        },
    }
    if location:
        out[family]["trak"] = tracking(path, font, location)
    if family in KERN_TARGETS:
        cps = TTFont(os.path.join(a.targets, KERN_TARGETS[family])).getBestCmap().keys()
    else:
        cps = KERN_RANGE
    out[family]["kern"] = kerning(font, cps)
    kern = sum(map(len, out[family]["kern"].values()))
    print(f"{family:20} upem={out[family]['upem']:5} advances={len(out[family]['advances']):5} kern={kern:5}")

os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w") as fh:
    json.dump(out, fh, separators=(",", ":"), sort_keys=True)
print(f"wrote {dest} ({os.path.getsize(dest) // 1024} KB)")
