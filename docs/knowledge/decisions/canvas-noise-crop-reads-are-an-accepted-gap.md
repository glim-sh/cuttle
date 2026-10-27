---
type: Decision
title: Canvas noise on a cropped read is an accepted gap
description: Seed-stable canvas noise is keyed on the buffer a read returns, so a crop, a 1x1 read or a float16 read of the same pixels is noised differently from the full read; this was accepted as a known gap rather than redesigning the noise per pixel, because no known detector compares such reads and full reads stay consistent.
tags: [stealth, canvas, noise, known-gap]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-27T16:30:00+00:00" }
sources:
  - id: maintainer
    resource: "maintainer decision during the 154 release review, 2026-09-29 (question Q45: keep the noise, ship the crop case as a known gap)"
    title: Maintainer decision
  - id: p55
    resource: /packages/browser/patches/0055-canvas-noise-seed-stable.patch
    title: Patch 0055 - seed-stable canvas noise
  - id: plan
    resource: /docs/plans/2609-29-stealth-improvements-after-154.md
    title: 154 plan, known limits - "Canvas noise is keyed on the requested rect"
---

# Decision

Patch 0055 keys its canvas noise on the seed and a hash of the buffer a read
returns, so repeated full reads of an unchanged canvas agree. A read of a
different shape - a crop, a 1x1 pixel, a float16 read - hashes different bytes
and gets different noise, so it disagrees with the matching region of the full
read.[^p55] That stays as a known gap.[^maintainer][^plan]

# Why not fix it

The alternative was noise computed per pixel from the seed, the pixel's
position and its neighbours. At the readback site the patch receives only the
returned buffer and its layout, not the read's offset in the canvas, so a
per-pixel key needs that offset passed down through every readback and encoder
path - more surface and more risk in a patch series that must be rebased on
every Chromium bump. No detector known at the time compares a cropped read with
a full one, and the full-read consistency that detectors do check is kept.[^maintainer]

# When to revisit

When a detector is seen comparing differently shaped reads of one canvas, or
when 0055 is redesigned anyway. A change to the noise key for another reason
(such as the Windows first-read rounding, see
[the raster-path finding](/findings/container-canvas-raster-path.md)) does not
by itself close this gap: a crop is still a different buffer.

[^maintainer]: Maintainer decision
[^p55]: Patch 0055 - seed-stable canvas noise
[^plan]: 154 plan, known limits - "Canvas noise is keyed on the requested rect"
