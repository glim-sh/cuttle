---
type: Finding
title: Container 2D canvas rasterizes like a Mac, not like Windows
description: In cuttle's container, 2D canvas runs Ganesh GL on ANGLE over Mesa llvmpipe and antialiases paths with 4x MSAA, which is what real Mac Chrome draws (CreepJS low-entropy arc 128/191/64); real Windows Chrome draws it with analytic AA (178/247/56), the path Chrome takes under its Intel msaa_is_slow driver workaround; passing --msaa_is_slow reproduces Windows exactly, and CPU Skia (192/244/53) matches neither.
tags: [stealth, canvas, skia, gpu, windows, macos, creepjs]
status: stable
stale_after: "2027-09-01T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T18:35:00+00:00" }
sources:
  - id: measure
    resource: "Measurement on 2026-09-29: stealth-Chromium 151 and 154 in the amd64 and arm64 images, browser launched by cuttle serve; CreepJS's low-entropy canvas (lowEntropyImageData) with and without --msaa_is_slow, and with GPU raster disabled; raster backend Ganesh GL on ANGLE over Mesa llvmpipe, maxMsaaSamples 4"
    title: Container canvas measurements
  - id: realref
    resource: "benches/realref.py on real Chrome 154.0.8037.58 on a Mac (Apple Silicon) and on a Windows 11 PC, 2026-09-29"
    title: Real-Chrome 154 baselines
  - id: sharedctx
    resource: https://github.com/chromium/chromium/blob/9d414657c043ed805664bd3d0d9e3081ee3e13e2/gpu/command_buffer/service/shared_context_state.cc
    title: SharedContextState - GrContextOptions.fAllowMSAAOnNewIntel = !MSAAIsSlow(workarounds)
  - id: skiautils
    resource: https://github.com/chromium/chromium/blob/9d414657c043ed805664bd3d0d9e3081ee3e13e2/gpu/command_buffer/service/skia_utils.cc
    title: skia_utils.cc - msaa_is_slow is true for all Intel GPUs
  - id: args
    resource: /packages/cuttle/internal/fingerprint/args.go
    title: --msaa_is_slow on the Windows persona only
---

# Container 2D canvas rasterizes like a Mac, not like Windows

## What the container draws

With no GPU, cuttle's container runs 2D canvas as Ganesh GL on ANGLE over
Mesa llvmpipe, which offers 4x MSAA. Skia then draws antialiased paths such
as CreepJS's low-entropy arc with its MSAA path renderer.[^measure] The
resulting pixels (lowEntropyImageData 128/191/64) are exactly what real Mac
Chrome 154 produces, so the macOS persona needs nothing.[^realref] 151 had the
same result.[^measure]

## Why real Windows differs

Chrome marks every Intel GPU with the `msaa_is_slow` driver-bug
workaround,[^skiautils] and with it set, Ganesh is told not to use MSAA on
Intel (`fAllowMSAAOnNewIntel` is false).[^sharedctx] Skia then draws the
same arc with analytic AA, which is what real Windows Chrome 154 reads:
178/247/56.[^realref] The difference comes from the raster path, not from the
GPU vendor strings, so spoofing the renderer string cannot fix it.

## The fix and what does not work

`--msaa_is_slow` on the command line applies the same workaround. In the
container it reproduces the Windows pixels exactly, with no side effects on
WebGL, so cuttle passes it on the Windows persona only.[^measure][^args]
Turning off GPU raster (CPU Skia) is not an alternative: it draws 192/244/53,
which matches neither real platform.[^measure]

The workaround is Intel's, so the flag fits a persona that claims an Intel
GPU; for the Windows pool's AMD row, real Chrome's raster path is not
measured. Any change of X server or GL backend needs this probe re-run,
because the pixels follow whatever MSAA support the backend offers. VNC mode
no longer forces `--use-angle=swiftshader`: KasmVNC's Xvnc serves GLX like
Xvfb, so both modes take the same ANGLE-on-llvmpipe path and draw the same
pixels.[^measure]

[^measure]: Container canvas measurements
[^realref]: Real-Chrome 154 baselines
[^sharedctx]: SharedContextState - fAllowMSAAOnNewIntel = !MSAAIsSlow(workarounds)
[^skiautils]: skia_utils.cc - msaa_is_slow is true for all Intel GPUs
[^args]: --msaa_is_slow on the Windows persona only
