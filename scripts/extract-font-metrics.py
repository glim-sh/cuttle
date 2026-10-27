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
    "Lucida Grande Bold": ("LucidaGrande.ttc", 1, None),
    "Geneva": ("Geneva.ttf", None, None),
    "Helvetica Neue": ("HelveticaNeue.ttc", 0, None),
    "Helvetica Neue Bold": ("HelveticaNeue.ttc", 1, None),
    "Monaco": ("Monaco.ttf", None, None),
    "Helvetica": ("Helvetica.ttc", 0, None),
    "Helvetica Bold": ("Helvetica.ttc", 1, None),
    "Times": ("Times.ttc", 0, None),
    "Times Bold": ("Times.ttc", 1, None),
    "Courier": ("Courier.ttc", 0, None),
    "Courier Bold": ("Courier.ttc", 1, None),
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
    "Avenir Bold": ("Avenir.ttc", 4, None),  # Heavy: what bold Avenir picks
    "Avenir Next": ("Avenir Next.ttc", 7, None),
    "Avenir Next Bold": ("Avenir Next.ttc", 0, None),
    "Futura": ("Supplemental/Futura.ttc", 0, None),
    "Futura Bold": ("Supplemental/Futura.ttc", 2, None),
    # The generic cursive, fantasy and math defaults of Mac Chrome.
    "Apple Chancery": ("Supplemental/Apple Chancery.ttf", None, None),
    "Papyrus": ("Supplemental/Papyrus.ttc", 1, None),
    "STIX Two Math": ("Supplemental/STIXTwoMath.otf", None, None),
    # Faces font-detection scripts probe for (FingerprintJS by rendered width,
    # CreepJS by local() full name to date the macOS release).
    "Arial Unicode MS": ("Supplemental/Arial Unicode.ttf", None, None),
    "Gill Sans": ("Supplemental/GillSans.ttc", 0, None),
    "Kohinoor Devanagari Medium": ("Kohinoor.ttc", 1, None),
    "Luminari": ("Supplemental/Luminari.ttf", None, None),
    "American Typewriter Semibold": ("Supplemental/AmericanTypewriter.ttc", 3, None),
    "SignPainter-HouseScript Semibold": ("Supplemental/SignPainter.ttc", 1, None),
    "InaiMathi Bold": ("Supplemental/InaiMathi-MN.ttc", 1, None),
    "Galvji": ("Supplemental/Galvji.ttc", 0, None),
    "MuktaMahee Regular": ("MuktaMahee.ttc", 0, None),
    "Noto Sans Gunjala Gondi": ("Supplemental/NotoSansGunjalaGondi-Regular.otf", None, None),
    "Noto Sans Masaram Gondi": ("Supplemental/NotoSansMasaramGondi-Regular.otf", None, None),
    "Noto Serif Yezidi": ("Supplemental/NotoSerifYezidi-Regular.otf", None, None),
    "Apple SD Gothic Neo ExtraBold": ("AppleSDGothicNeo.ttc", 14, None),
    "STIX Two Text": ("Supplemental/STIXTwoText.ttf", None, {"wght": 400}),
}

# "Segoe UI" is the Windows system font behind CSS system-ui. It carries no trak
# or STAT table, so its advances alone reproduce real Chrome's widths. Past the
# families the pack had from the start, the table covers the rest of a stock
# Windows 10/11 core font set and the faces detectors probe beyond it, each face
# real Windows ships for them (Sitka Small and Bahnschrift are named instances of
# variable fonts).
WINDOWS = {
    "Segoe UI": ("segoeui.ttf", None, None),
    "Segoe UI Bold": ("segoeuib.ttf", None, None),
    "Segoe UI Italic": ("segoeuii.ttf", None, None),
    "Segoe UI Bold Italic": ("segoeuiz.ttf", None, None),
    "Segoe UI Light": ("segoeuil.ttf", None, None),
    "Segoe UI Light Italic": ("seguili.ttf", None, None),
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
    "Georgia Italic": ("georgiai.ttf", None, None),
    "Georgia Bold Italic": ("georgiaz.ttf", None, None),
    "Trebuchet MS Italic": ("trebucit.ttf", None, None),
    "Trebuchet MS Bold Italic": ("trebucbi.ttf", None, None),
    "Verdana Italic": ("verdanai.ttf", None, None),
    "Verdana Bold Italic": ("verdanaz.ttf", None, None),
    "Consolas Italic": ("consolai.ttf", None, None),
    "Consolas Bold Italic": ("consolaz.ttf", None, None),
    "Comic Sans MS Italic": ("comici.ttf", None, None),
    "Comic Sans MS Bold Italic": ("comicz.ttf", None, None),
    "Calibri": ("calibri.ttf", None, None),
    "Calibri Bold": ("calibrib.ttf", None, None),
    "Calibri Italic": ("calibrii.ttf", None, None),
    "Calibri Bold Italic": ("calibriz.ttf", None, None),
    "Cambria": ("cambria.ttc", 0, None),
    "Cambria Bold": ("cambriab.ttf", None, None),
    "Cambria Italic": ("cambriai.ttf", None, None),
    "Cambria Bold Italic": ("cambriaz.ttf", None, None),
    "Cambria Math": ("cambria.ttc", 1, None),
    "Candara": ("candara.ttf", None, None),
    "Candara Bold": ("candarab.ttf", None, None),
    "Candara Italic": ("candarai.ttf", None, None),
    "Candara Bold Italic": ("candaraz.ttf", None, None),
    "Constantia": ("constan.ttf", None, None),
    "Constantia Bold": ("constanb.ttf", None, None),
    "Constantia Italic": ("constani.ttf", None, None),
    "Constantia Bold Italic": ("constanz.ttf", None, None),
    "Corbel": ("corbel.ttf", None, None),
    "Corbel Bold": ("corbelb.ttf", None, None),
    "Corbel Italic": ("corbeli.ttf", None, None),
    "Corbel Bold Italic": ("corbelz.ttf", None, None),
    "Palatino Linotype": ("pala.ttf", None, None),
    "Palatino Linotype Bold": ("palab.ttf", None, None),
    "Palatino Linotype Italic": ("palai.ttf", None, None),
    "Palatino Linotype Bold Italic": ("palabi.ttf", None, None),
    "Sitka Small": ("SitkaVF.ttf", None, {"opsz": 6, "wght": 400}),
    "Sitka Small Bold": ("SitkaVF.ttf", None, {"opsz": 6, "wght": 700}),
    "Sitka Small Italic": ("SitkaVF-Italic.ttf", None, {"opsz": 6, "wght": 400}),
    "Sitka Small Bold Italic": ("SitkaVF-Italic.ttf", None, {"opsz": 6, "wght": 700}),
    "Bahnschrift": ("bahnschrift.ttf", None, {"wght": 400, "wdth": 100}),
    "Bahnschrift Bold": ("bahnschrift.ttf", None, {"wght": 700, "wdth": 100}),
    "Franklin Gothic Medium": ("framd.ttf", None, None),
    "Franklin Gothic Medium Italic": ("framdit.ttf", None, None),
    "Ebrima": ("ebrima.ttf", None, None),
    "Ebrima Bold": ("ebrimabd.ttf", None, None),
    "Gadugi": ("gadugi.ttf", None, None),
    "Gadugi Bold": ("gadugib.ttf", None, None),
    "Malgun Gothic": ("malgun.ttf", None, None),
    "Malgun Gothic Bold": ("malgunbd.ttf", None, None),
    "Microsoft JhengHei": ("msjh.ttc", 0, None),
    "Microsoft JhengHei Bold": ("msjhbd.ttc", 0, None),
    "Microsoft JhengHei UI": ("msjh.ttc", 1, None),
    "Microsoft JhengHei UI Bold": ("msjhbd.ttc", 1, None),
    "Microsoft New Tai Lue": ("ntailu.ttf", None, None),
    "Microsoft New Tai Lue Bold": ("ntailub.ttf", None, None),
    "Microsoft PhagsPa": ("phagspa.ttf", None, None),
    "Microsoft PhagsPa Bold": ("phagspab.ttf", None, None),
    "Microsoft Tai Le": ("taile.ttf", None, None),
    "Microsoft Tai Le Bold": ("taileb.ttf", None, None),
    "Myanmar Text": ("mmrtext.ttf", None, None),
    "Myanmar Text Bold": ("mmrtextb.ttf", None, None),
    "Nirmala UI": ("Nirmala.ttc", 0, None),
    "Nirmala UI Bold": ("Nirmala.ttc", 1, None),
    "Segoe Print": ("segoepr.ttf", None, None),
    "Segoe Print Bold": ("segoeprb.ttf", None, None),
    "Segoe Script": ("segoesc.ttf", None, None),
    "Segoe Script Bold": ("segoescb.ttf", None, None),
    "Gabriola": ("Gabriola.ttf", None, None),
    "Ink Free": ("Inkfree.ttf", None, None),
    "Javanese Text": ("javatext.ttf", None, None),
    "Lucida Sans Unicode": ("l_10646.ttf", None, None),
    "Marlett": ("marlett.ttf", None, None),
    "Microsoft Himalaya": ("himalaya.ttf", None, None),
    "Microsoft Sans Serif": ("micross.ttf", None, None),
    "Microsoft Yi Baiti": ("msyi.ttf", None, None),
    "MingLiU-ExtB": ("mingliub.ttc", 0, None),
    "PMingLiU-ExtB": ("mingliub.ttc", 1, None),
    "MingLiU_HKSCS-ExtB": ("mingliub.ttc", 2, None),
    "Mongolian Baiti": ("monbaiti.ttf", None, None),
    "MS Gothic": ("msgothic.ttc", 0, None),
    "MS UI Gothic": ("msgothic.ttc", 1, None),
    "MS PGothic": ("msgothic.ttc", 2, None),
    "MV Boli": ("mvboli.ttf", None, None),
    "Segoe MDL2 Assets": ("segmdl2.ttf", None, None),
    "Segoe Fluent Icons": ("SegoeIcons.ttf", None, None),
    "SimSun": ("simsun.ttc", 0, None),
    "SimSun-ExtB": ("simsunb.ttf", None, None),
    "Sylfaen": ("sylfaen.ttf", None, None),
    "Symbol": ("symbol.ttf", None, None),
    "Webdings": ("webdings.ttf", None, None),
    "Microsoft YaHei": ("msyh.ttc", 0, None),
    "Microsoft YaHei Bold": ("msyhbd.ttc", 0, None),
    "Yu Gothic": ("YuGothR.ttc", 0, None),
    "Yu Gothic Bold": ("YuGothB.ttc", 0, None),
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

# Families whose advances are kept to the Latin, Greek and Cyrillic a width
# probe renders, not their tens of thousands of CJK or symbol codepoints.
LATIN_ONLY = {
    "Arial Unicode MS", "Apple SD Gothic Neo ExtraBold", "STIX Two Math",
    "Malgun Gothic", "Malgun Gothic Bold", "Microsoft JhengHei", "Microsoft JhengHei Bold",
    "Microsoft JhengHei UI", "Microsoft JhengHei UI Bold", "MingLiU-ExtB", "PMingLiU-ExtB",
    "MingLiU_HKSCS-ExtB", "MS Gothic", "MS UI Gothic", "MS PGothic", "SimSun", "SimSun-ExtB",
    "Microsoft YaHei", "Microsoft YaHei Bold", "Yu Gothic", "Yu Gothic Bold",
    "Segoe MDL2 Assets", "Segoe Fluent Icons",
}
LATIN_END = 0x530

PLATFORMS = {
    "macos": (MACOS, "/System/Library/Fonts", "ops/docker/macfonts/metrics.json"),
    "windows": (WINDOWS, "C:/Windows/Fonts", "ops/docker/winfonts/metrics.json"),
}

ASCII = range(0x20, 0x7F)
# A symbol-encoded font (Webdings, Symbol, Marlett) maps its glyphs at
# U+F020-F0FF, and Windows renders U+0020-00FF text with them, so its advances
# are keyed by the Latin codepoint.
SYMBOL_BASE = 0xF000
# Words DirectWrite keeps out of a face's family name, so "Franklin Gothic
# Medium" is family "Franklin Gothic" in Chrome; the Bold and Italic words make
# the legacy (RIBBI) style.
WEIGHT_WORDS = {"Thin", "ExtraLight", "Light", "Semilight", "SemiLight", "Regular", "Medium",
                "Semibold", "SemiBold", "Bold", "ExtraBold", "Black", "Italic"}
GDEF_MARK_CLASS = 3
KERN_HORIZONTAL, KERN_CROSS_STREAM = 0x1, 0x4  # legacy kern subtable coverage bits
KERN_APPLE_NOT_HORIZONTAL = 0xE0
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


def best_cmap(font):
    cmap = font.getBestCmap()
    if not cmap and (symbol := font["cmap"].getcmap(3, 0)):
        cmap = {cp - SYMBOL_BASE: g for cp, g in symbol.cmap.items() if SYMBOL_BASE <= cp <= SYMBOL_BASE + 0xFF}
    return cmap


def windows_names(full, ps, weight):
    """A face as Windows Chrome names it: local() matches the full or PostScript
    name, CSS the family (DirectWrite's, without the weight words), and the
    legacy family plus Bold/Italic style is the RIBBI naming of the real file."""
    words = full.split()
    legacy = " ".join(w for w in words if w not in ("Regular", "Bold", "Italic"))
    return {
        "family": " ".join(w for w in words if w not in WEIGHT_WORDS),
        "legacy": legacy,
        "style": " ".join(w for w in words if w in ("Bold", "Italic")) or "Regular",
        "full": full,
        "ps": ps,
        "weight": weight,
    }


def ligatures(font, cps):
    """The two-codepoint Latin ligatures HarfBuzz forms by default (liga, clig)
    as {(left, right): ligature glyph}, so their width can be kept as a kerning
    pair on a stand-in that forms none (longer ligatures are not reproduced)."""
    if "GSUB" not in font:
        return {}
    gsub = font["GSUB"].table
    scripts = {r.ScriptTag: r.Script for r in gsub.ScriptList.ScriptRecord}
    script = scripts.get("latn") or scripts.get("DFLT")
    langsys = script and script.DefaultLangSys
    if not langsys:
        return {}
    records = gsub.FeatureList.FeatureRecord
    indices = sorted({
        i for f in langsys.FeatureIndex if records[f].FeatureTag in ("liga", "clig")
        for i in records[f].Feature.LookupListIndex
    })
    cp_of = {g: cp for cp, g in best_cmap(font).items() if cp in cps}
    out = {}
    for i in indices:
        lookup = gsub.LookupList.Lookup[i]
        for sub in lookup.SubTable:
            sub = sub.ExtSubTable if lookup.LookupType == 7 else sub
            if sub.LookupType != 4:
                continue
            for first, ligs in sub.ligatures.items():
                for lig in ligs:
                    if len(lig.Component) == 1 and first in cp_of and lig.Component[0] in cp_of:
                        out.setdefault((cp_of[first], cp_of[lig.Component[0]]), lig.LigGlyph)
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
    cmap = best_cmap(font)
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
            if getattr(st, "format", None) != 0:  # AAT state-table kerning: not reproduced
                continue
            if getattr(st, "apple", False):  # Apple's kern: vertical/cross-stream/variation bits
                horizontal = not st.coverage & KERN_APPLE_NOT_HORIZONTAL
            else:
                horizontal = st.coverage & KERN_HORIZONTAL and not st.coverage & KERN_CROSS_STREAM
            if horizontal:
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
    names = None
    if location:
        # A variable font's faces are its named instances; local() matches the
        # instance's full name ("STIX Two Text Regular").
        var = TTFont(path)
        inst = next((i for i in var["fvar"].instances if i.coordinates == location), None)
        if inst:
            vn = var["name"]
            fam = vn.getDebugName(16) or vn.getDebugName(1)
            ps = vn.getDebugName(inst.postscriptNameID) if inst.postscriptNameID != 0xFFFF else None
            names = {"family": fam, "full": f"{fam} {vn.getDebugName(inst.subfamilyNameID)}",
                     "ps": ps or fam.replace(" ", "") + "-" + vn.getDebugName(inst.subfamilyNameID).replace(" ", "")}
        font = instance(path, location)
    else:
        font = TTFont(path, fontNumber=index) if index is not None else TTFont(path)
    hmtx, cmap = font["hmtx"], best_cmap(font)
    hhea = font["hhea"]
    # Courier.ttc has no OS/2 table; CoreText sizes it from hhea alone.
    os2 = font["OS/2"] if "OS/2" in font else None
    ascent, descent, line_gap = hhea.ascent, hhea.descent, hhea.lineGap
    typo_line_gap = os2.sTypoLineGap if os2 else hhea.lineGap
    if a.platform == "windows":
        # Windows sizes a font from its win metrics (typo ones when USE_TYPO_METRICS
        # is set), the Linux build from hhea, and real Windows Chrome lays out a
        # line with no line gap (measured on Mongolian Baiti, Ebrima, MingLiU-ExtB,
        # Gabriola): carry those in hhea with both gaps zero.
        if os2.fsSelection & 0x80:
            ascent, descent, typo_line_gap = os2.sTypoAscender, os2.sTypoDescender, 0
        else:
            ascent, descent = os2.usWinAscent, -os2.usWinDescent
        line_gap = 0
        full = names["full"] if names else font["name"].getDebugName(4)
        if location:
            full = " ".join(w for w in full.split() if w != "Regular")
        ps = names["ps"] if names else font["name"].getDebugName(6)
        weight = location["wght"] if location else os2.usWeightClass
        names = windows_names(full, ps, int(weight))
    out[family] = {
        "upem": font["head"].unitsPerEm,
        "hhea": {"ascent": ascent, "descent": descent, "lineGap": line_gap},
        "os2": {
            "typoAscender": os2.sTypoAscender if os2 else hhea.ascent,
            "typoDescender": os2.sTypoDescender if os2 else hhea.descent,
            "typoLineGap": typo_line_gap,
            "winAscent": os2.usWinAscent if os2 else hhea.ascent,
            "winDescent": os2.usWinDescent if os2 else -hhea.descent,
            # CoreText sizes a font from hhea whatever this bit says; Windows
            # and FreeType switch to the typo metrics when it is set.
            "useTypoMetrics": a.platform == "windows" and bool(os2.fsSelection & 0x80),
        },
        "advances": {
            str(cp): hmtx.metrics[g][0]
            for cp, g in sorted(cmap.items())
            if g in hmtx.metrics and (family not in LATIN_ONLY or cp < LATIN_END)
        },
        # local() matches a face by its full or PostScript name.
        "names": names or {
            "family": font["name"].getDebugName(16) or font["name"].getDebugName(1),
            "full": font["name"].getDebugName(4),
            "ps": font["name"].getDebugName(6),
        },
    }
    if location and "trak" in font:
        out[family]["trak"] = tracking(path, font, location)
    if family in KERN_TARGETS:
        cps = TTFont(os.path.join(a.targets, KERN_TARGETS[family])).getBestCmap().keys()
    else:
        cps = KERN_RANGE
    out[family]["kern"] = kerning(font, cps)
    if a.platform == "windows":
        # The stand-in forms no ligature of its own (rename-fonts.py), so each
        # real one is the pair's kerning: a ligature hides the pair's own kern.
        adv = out[family]["advances"]
        for (left, right), glyph in ligatures(font, cps).items():
            row = out[family]["kern"].setdefault(str(left), {})
            delta = hmtx[glyph][0] - adv[str(left)] - adv[str(right)]
            if delta:
                row[str(right)] = delta
            else:
                row.pop(str(right), None)
            if not row:
                del out[family]["kern"][str(left)]
        # Its coverage is the real face's too: Windows renders any other
        # codepoint from a fallback font (Marlett has 39 glyphs). A Latin-only
        # entry lists the rest of it as codepoint ranges.
        out[family]["exact"] = True
        if family in LATIN_ONLY:
            ranges = []
            for cp in sorted(cmap):
                if ranges and cp == ranges[-1][1] + 1:
                    ranges[-1][1] = cp
                else:
                    ranges.append([cp, cp])
            out[family]["coverage"] = ranges
    kern = sum(map(len, out[family]["kern"].values()))
    print(f"{family:20} upem={out[family]['upem']:5} advances={len(out[family]['advances']):5} kern={kern:5}")

os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w") as fh:
    json.dump(out, fh, separators=(",", ":"), sort_keys=True)
print(f"wrote {dest} ({os.path.getsize(dest) // 1024} KB)")
