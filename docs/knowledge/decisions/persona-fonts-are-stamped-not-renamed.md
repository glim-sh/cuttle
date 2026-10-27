---
type: Decision
title: Persona font gaps are closed with metrics stamped from the real fonts, never bare renames
description: A family a persona lacks is added as a free font carrying the real face's advances, vertical metrics, kerning, ligature widths, coverage, style and names (integers only, extracted from the real font), never as a free font that is only renamed; which free font it is built from decides only the glyph shapes, so families may share a source.
tags: [fonts, stealth, windows, persona]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-27T17:00:00+00:00" }
sources:
  - id: issue
    resource: https://github.com/glim-sh/cuttle/issues/138
    title: "Windows persona exposes 18 of the 55 core Windows font families"
  - id: pr141
    resource: https://github.com/glim-sh/cuttle/pull/141
    title: "feat(docker): Windows persona exposes the full core font set (unstamped stand-ins, superseded)"
  - id: rename
    resource: /scripts/rename-fonts.py
    title: rename-fonts.py --metrics
  - id: extract
    resource: /scripts/extract-font-metrics.py
    title: extract-font-metrics.py windows
  - id: readme
    resource: /ops/docker/winfonts/README.md
    title: winfonts mapping and known gaps
  - id: measure
    resource: "Probe page measuring every core family (4 styles, 72px canvas widths, font box, line height, local() names) in real Chrome 156 on Windows 11 and in the amd64 image, 2026-10-27"
    title: real-vs-image font measurement
---

# Decision

When a persona lacks a font family that a real install has, the family is added
by stamping the real face's metrics onto a free font:[^rename][^extract] advance
widths, the vertical metrics Windows lays a line out with, kerning, the widths
of its two-letter ligatures, its codepoint coverage, its style and weight, and
its full and PostScript names. A free font that is only renamed is never added.

## Why

Detectors find fonts by measuring text against the generic families, and
compare those widths and line heights with what the real font gives. A renamed
free font is "present", but its widths are not the real font's, so it reads as a
substitute.[^issue] Stamping makes the free font measure like the real one: on
the Windows persona every core family's regular face measures exactly like real
Chrome 156 on Windows (72px, two probe strings, font box and line
height).[^measure] Because the widths come from the stamp, the free source only
decides glyph shapes, so several families can share one source and the pack
grows little (the full 55-family Windows set plus the probed extras added about
2.5 MB to the pack).[^readme]

## Compared with unstamped stand-ins

[#141](https://github.com/glim-sh/cuttle/pull/141) closed the same gap with 37
unstamped stand-ins, each from a different free font, with Noto script fonts
merged onto Latin faces, so that no two families shared metrics (+56 MB).[^pr141]
Those families were present but measured like their sources, not like Windows,
and the uniqueness constraint forced a new source for every family. Real fonts do
share metrics (Ebrima, Gadugi and Nirmala UI have Segoe UI's Latin; SimSun and
MS Gothic both measure 36px per Latin character at 72px), so pairwise
distinctness was the wrong target: the target is the real font.

## What the stamp has to carry on Windows

Measured, not assumed:[^measure]

- Windows Chrome lays a line out from the win metrics (the typo ones when
  USE_TYPO_METRICS is set) with no line gap at all; the Linux build reads hhea,
  so the stamp writes those values into hhea with both gaps zero.
- A free font's own ligatures (`fi`, `fl`) and contextual alternates replace the
  stamped glyphs, so they are dropped and the real face's ligatures become
  kerning pairs; the codepoints the real face lacks are dropped too, so they
  fall back as on Windows.
- CSS matches by the real face's style and weight, and `local()` by its full and
  PostScript names ("Franklin Gothic" is the family of "Franklin Gothic Medium";
  "Segoe UI Light" is a weight-300 face of "Segoe UI").

Left as known gaps: Windows widens a synthesized bold of some single-weight
fonts, Segoe Script's contextual alternates, and FreeType's native hinting of any
family named "MingLiU".[^readme]

[^issue]: Windows persona exposes 18 of the 55 core Windows font families
[^pr141]: feat(docker): Windows persona exposes the full core font set (unstamped stand-ins, superseded)
[^rename]: rename-fonts.py --metrics
[^extract]: extract-font-metrics.py windows
[^readme]: winfonts mapping and known gaps
[^measure]: real-vs-image font measurement
