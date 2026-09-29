---
type: Finding
title: How real Chrome resolves and measures the system font
description: In real Chrome 154, document.fonts.check() answers true for every local family so it cannot detect fonts; -apple-system is not recognised on any platform; macOS system-ui is SF at optical size 17 plus the font's per-size trak tracking, applied by HarfBuzz - which a Linux build reproduces only with a font that carries both trak and STAT; Windows system-ui is Segoe UI, whose plain advances plus GPOS kerning give its width exactly.
tags: [stealth, fonts, system-ui, macos, windows, measureText, harfbuzz]
status: stable
stale_after: "2027-09-01T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T17:20:00+00:00" }
sources:
  - id: realmac
    resource: "Measurement: real Chrome 154.0.8037.58 on macOS 27.2 (Apple Silicon), headed, driven over CDP; canvas measureText of 'The quick brown fox jumps over the lazy dog 0123456789' at 10-72px, 400 and 700"
    title: Real macOS Chrome 154 font measurements
  - id: realwin
    resource: "Measurement: real Chrome 154 on a Windows 11 PC in the logged-in console session, driven over CDP; canvas measureText of the same test string in system-ui and \"Segoe UI\" at 13, 16 and 72px, 400 and 700, kerning auto and none"
    title: Real Windows Chrome 154 font measurements
  - id: segoe
    resource: "C:\\Windows\\Fonts\\segoeui*.ttf (Segoe UI 5.72) on Windows 11, read with fontTools and shaped with HarfBuzz"
    title: Segoe UI tables
  - id: selawik
    resource: https://github.com/microsoft/Selawik/releases/tag/1.01
    title: Selawik 1.01, Microsoft's OFL Segoe UI stand-in
  - id: sfns
    resource: "/System/Library/Fonts/SFNS.ttf on macOS 27.2, read with fontTools (fvar axes, hmtx, trak)"
    title: SFNS.ttf tables
  - id: blinkptem
    resource: https://source.chromium.org/chromium/chromium/src/+/main:third_party/blink/renderer/platform/fonts/shaping/harfbuzz_face.cc
    title: HarfBuzzFace::GetScaledFont sets hb_font_set_ptem to the CSS px size
  - id: hbtrak
    resource: https://github.com/harfbuzz/harfbuzz/blob/main/src/hb-ot-shape.cc
    title: HarfBuzz applies trak only when the face also has a STAT table
  - id: fontcache
    resource: https://source.chromium.org/chromium/chromium/src/+/main:third_party/blink/renderer/platform/fonts/font_cache.cc
    title: FontCache::GetFontPlatformData / SystemFontPlatformData
  - id: patch
    resource: /packages/browser/patches/0063-system-ui-persona-font.patch
    title: Patch 0063 - system-ui is the persona's system font
---

# How real Chrome resolves and measures the system font

## document.fonts.check() is not a font detector

In real Chrome, `document.fonts.check("16px <family>")` returns `true` for
every local family, including one that does not exist
(`"NoSuchFontXyz"`).[^realmac] Per spec it reports whether the matching faces
of the document's FontFaceSet are loaded; a local family is not in that set, so
nothing needs loading. Font presence is only observable through measurement:
`measureText` or layout width against the fallback. Probes and smoke gates must
measure; a `check()`-based probe passes with no fonts at all.

## -apple-system is not a Chrome family

On real Chrome for macOS, `-apple-system`, `".SF NS"` and `".SFNS-Regular"`
measure exactly like an unknown family (376.375px at 16px, the default serif
fallback); the same holds on Windows.[^realmac][^realwin] Only Safari knows
`-apple-system`. Mapping it to any face - the Dockerfile once pinned it to
Helvetica (410.03px) - creates a tell. `BlinkMacSystemFont`, by contrast, is the
system font on macOS (case-sensitive: `blinkmacsystemfont` falls back, and
computed style reports `"system-ui"`), and unknown on Windows.

## macOS system-ui is SF at optical size 17 plus trak

`system-ui` on real Mac Chrome is SFNS.ttf, whose `opsz` axis runs 17-96 with
default 28.[^sfns] Chrome sets `opsz` to the CSS px size, so every size up to
17px renders the opsz-17 ("SF Pro Text") instance. On top of that HarfBuzz
applies SF's `trak` table: Blink passes the CSS px size as `ptem`,[^blinkptem]
and at 16px each glyph is 40 units (of 2048) tighter, at 13px 12 units - the
opsz-17 advances plus that tracking reproduce the measured unkerned width
exactly.[^realmac] Real widths on the test string: 351.622px at 13px and
420.953px at 16px (weight 400); 373.506 and 447.887 at 700.

A Linux build gets the same tracking for free from any font that carries a
`trak` table - but only if the face also has a `STAT` table, which HarfBuzz
requires before it applies `trak`.[^hbtrak]

On Linux, Blink resolves `system-ui` itself (the GTK default font, or
`--system-font-family`) before fontconfig sees the name, so no fontconfig rule
can redirect it; patch 0063 does it in `FontCache` instead.[^fontcache][^patch]

## Windows system-ui is Segoe UI, advances plus kerning

Real Windows `system-ui` measures exactly like `"Segoe UI"` by name, at every
size and weight.[^realwin] Segoe UI 5.72 has no `trak` or `STAT` table and no
optical sizes, so no size-dependent spacing applies: its hmtx advances,
HarfBuzz-shaped with its GPOS kerning, reproduce real Chrome's widths exactly
(measureText kerns by default).[^segoe] On the test string, weight 400 is
331.34765625px at 13px and 407.8125 at 16px, with no kerning pair in it; weight
700 is 354.0532 and 435.7578, 0.12% below its unkerned 354.4912 and 436.2969.
A kerning-heavy string ("AVAWAY To Ta Te Yo LT") is 7.7% narrower kerned at
400.[^realwin]

Selawik 1.01, Microsoft's OFL stand-in for Segoe UI, has Segoe UI's Latin
advances exactly but only 348 codepoints (Segoe UI has about 4000, with Greek
and Cyrillic), a shorter hhea ascent (2027 against 2210), and no kerning at
all.[^selawik][^segoe] Built from Selawik with Segoe UI's advances and vertical
metrics stamped on, the Windows pack's "Segoe UI" measures exactly like real
Chrome on the test string at weight 400 and 0.12% wide at 700; kerned text
stays wide by up to about 8%, and Cyrillic in it falls back to another pack
font (6% narrow on a Cyrillic pangram).

One measurement trap: in a long loop over sizes, real Mac Chrome once measured
32px/400 with opsz-17 advances (9.62px per em instead of 8.81); a fresh page
measured it normally. Take reference widths from a fresh page.[^realmac]

[^realmac]: Real macOS Chrome 154 font measurements
[^realwin]: Real Windows Chrome 154 font measurements
[^segoe]: Segoe UI tables
[^selawik]: Selawik 1.01, Microsoft's OFL Segoe UI stand-in
[^sfns]: SFNS.ttf tables
[^blinkptem]: HarfBuzzFace::GetScaledFont sets hb_font_set_ptem to the CSS px size
[^hbtrak]: HarfBuzz applies trak only when the face also has a STAT table
[^fontcache]: FontCache::GetFontPlatformData / SystemFontPlatformData
[^patch]: Patch 0063 - system-ui is the persona's system font
