#!/usr/bin/env python3
"""Rename a font's internal `name` table so a free font presents the family name
a fingerprint profile is expected to expose (e.g. Arial<-Liberation Sans,
Calibri<-Carlito, "Segoe UI Emoji"<-Noto Color Emoji, "Microsoft YaHei"<-WenQuanYi
Zen Hei, Helvetica<-Liberation Sans, Menlo<-DejaVu Sans Mono). Font ENUMERATION
(canvas measureText, document.fonts, CSS font matching) then sees only the target
platform's family names while the actual glyph coverage - including color emoji
and CJK - is preserved so canvas-hash anti-bot checks still see real, coherent
rendering.

With --metrics, also stamp the target's METRICS: per-codepoint advance widths,
the hhea/OS-2 vertical metrics (with the USE_TYPO_METRICS bit that picks
between them) and, when the table has them, the size-dependent `trak` tracking
and the kerning pairs. This font is first rescaled to the table's upem, so the
values land exactly. The pairs replace the font's own kerning as a single GPOS
kern lookup; an empty set just removes it.
Renaming alone is not enough - detectors compare measureText widths against the
generics, and line-height comes from the vertical metrics, so a renamed font
keeping its own metrics still reads as a substitute. Only integers are copied,
never outlines, which is the basis on which Liberation and Nimbus were built.

Usage: rename-fonts.py <src> <target-family> <out> [--ttc-index N]
                       [--metrics metrics.json [--metrics-key KEY]]

Handles .ttf/.otf and a single face of a .ttc collection (--ttc-index),
including color-emoji (CBDT/CBLC/COLR) fonts - only the name table is rewritten.
"""

import argparse
import json

from fontTools.otlLib.builder import (
    LOOKUP_FLAG_IGNORE_MARKS,
    buildLookup,
    buildPairPosGlyphs,
    buildStatTable,
    buildValue,
)
from fontTools.ttLib import TTFont, newTable
from fontTools.ttLib.scaleUpem import scale_upem
from fontTools.ttLib.tables import otTables
from fontTools.ttLib.tables._t_r_a_k import TrackData, TrackTableEntry

p = argparse.ArgumentParser()
p.add_argument("src")
p.add_argument("target")
p.add_argument("out")
p.add_argument("--ttc-index", type=int, default=None)
p.add_argument("--metrics", help="JSON metrics table from extract-font-metrics.py")
p.add_argument("--metrics-key", help="entry of the metrics table (default: target)")
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
    key = a.metrics_key or target
    with open(a.metrics) as fh:
        table = json.load(fh)
    if key not in table:
        raise SystemExit(f"{a.metrics}: no metrics for {key!r}")
    m = table[key]
    # Rescale the stand-in to the real font's upem first, so every advance and
    # kerning value is copied exactly instead of rounded per glyph.
    upem = font["head"].unitsPerEm
    if upem != m["upem"]:
        scale_upem(font, m["upem"])

    hmtx, cmap, advances = font["hmtx"], font.getBestCmap(), m["advances"]
    stamped = 0
    for cp, gname in cmap.items():
        want = advances.get(str(cp))
        if want is not None and gname in hmtx.metrics:
            hmtx.metrics[gname] = (want, hmtx.metrics[gname][1])
            stamped += 1

    hhea, os2 = font["hhea"], font["OS/2"]
    hhea.ascent, hhea.descent = m["hhea"]["ascent"], m["hhea"]["descent"]
    hhea.lineGap = m["hhea"]["lineGap"]
    os2.sTypoAscender = m["os2"]["typoAscender"]
    os2.sTypoDescender = m["os2"]["typoDescender"]
    os2.sTypoLineGap = m["os2"]["typoLineGap"]
    os2.usWinAscent, os2.usWinDescent = m["os2"]["winAscent"], m["os2"]["winDescent"]
    if "useTypoMetrics" in m["os2"]:
        os2.fsSelection = os2.fsSelection & ~0x80 | (0x80 if m["os2"]["useTypoMetrics"] else 0)
    if "trak" in m:
        trak = newTable("trak")
        trak.version, trak.format = 1.0, 0
        track = {float(size): v for size, v in m["trak"].items()}
        trak.horizData = TrackData({0.0: TrackTableEntry(track, nameIndex=256)})
        trak.vertData = TrackData()
        font["trak"] = trak
        # HarfBuzz applies trak only to fonts that also carry a STAT table.
        if "STAT" not in font:
            buildStatTable(font, [{"tag": "wght", "name": "Weight"}])
    kerned = 0
    if "kern" in m:
        pairs = {}
        for left, row in m["kern"].items():
            for right, v in row.items():
                g1, g2 = cmap.get(int(left)), cmap.get(int(right))
                if g1 and g2:
                    pairs.setdefault((g1, g2), (buildValue({"XAdvance": v}), None))
        kerned = len(pairs)
        # HarfBuzz ignores the legacy kern table once GPOS has a kern feature,
        # but the font's own pairs must not survive anywhere.
        if "kern" in font:
            del font["kern"]
        if pairs and "GPOS" not in font:
            raise SystemExit(f"{a.src}: no GPOS table to carry {key!r} kerning")
    if "kern" in m and "GPOS" in font:
        gpos = font["GPOS"].table
        lookups = gpos.LookupList.Lookup
        # An empty table (the real font has no kerning) leaves the kern
        # features pointing at no lookup, which drops the font's own pairs.
        kern_lookups = []
        if pairs:
            lookups.append(buildLookup(buildPairPosGlyphs(pairs, font.getReverseGlyphMap()), flags=LOOKUP_FLAG_IGNORE_MARKS))
            gpos.LookupList.LookupCount = len(lookups)
            kern_lookups = [len(lookups) - 1]
        # Repoint every existing kern feature at the new lookup only, or add one;
        # every other feature and lookup is left as it was.
        records = gpos.FeatureList.FeatureRecord
        kern = [i for i, r in enumerate(records) if r.FeatureTag == "kern"]
        if not kern and pairs:
            rec = otTables.FeatureRecord()
            rec.FeatureTag, rec.Feature = "kern", otTables.Feature()
            rec.Feature.FeatureParams = None
            records.append(rec)
            gpos.FeatureList.FeatureCount = len(records)
            kern = [len(records) - 1]
        for i in kern:
            records[i].Feature.LookupListIndex = kern_lookups
            records[i].Feature.LookupCount = len(kern_lookups)
        for script in gpos.ScriptList.ScriptRecord if kern else ():
            langs = [script.Script.DefaultLangSys] + [r.LangSys for r in script.Script.LangSysRecord]
            for ls in filter(None, langs):
                if not any(records[i].FeatureTag == "kern" for i in ls.FeatureIndex):
                    ls.FeatureIndex.append(kern[0])
                    ls.FeatureCount = len(ls.FeatureIndex)
    note = f" [metrics {key}: {stamped} advances, {kerned} kern pairs, upem {upem}->{m["upem"]}]"

font.save(a.out)
print(f"{a.src} -> {target} ({a.out}){note}")
