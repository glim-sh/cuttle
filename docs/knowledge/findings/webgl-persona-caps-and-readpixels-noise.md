---
type: Finding
title: WebGL persona caps are Intel and AMD values, and readPixels noise must stay inside the validated bytes
description: The persona WebGL caps (MAX_VERTEX_UNIFORM_VECTORS 4096, MAX_SAMPLES 8 or 16 per device) are right for the Intel and AMD GPUs cuttle claims; an NVIDIA reference reads 4095 only because ANGLE's skipVSConstantRegisterZero is NVIDIA-only, so personas must not be "fixed" to match it; and noise written into a readPixels destination must be bounded by the bytes validation accepted for that format and type, never width*height*4.
tags: [stealth, webgl, angle, gpu, memory-safety]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T20:49:00+00:00" }
sources:
  - id: p16
    resource: /packages/browser/patches/0016-webgl-vendor-renderer-from-cli.patch
    title: Patch 0016 - persona WebGL identity, caps table (cuttle_webgl_caps.h) and readPixels noise
  - id: p58
    resource: /packages/browser/patches/0058-webgl-persona-gpu-caps.patch
    title: Patch 0058 - WebGL2 int64 caps from the same table
  - id: pool
    resource: /packages/cuttle/internal/fingerprint/args.go
    title: Windows persona GPU pool - Intel and AMD Direct3D11 adapters only
  - id: angle
    resource: https://github.com/google/angle/blob/0abe29721a6140839b2f2f9dc7c6cab65b5ac9a9/src/libANGLE/renderer/d3d/d3d11/renderer11_utils.cpp
    title: renderer11_utils.cpp - skipVSConstantRegisterZero enabled when isNvidia, reserving one vertex uniform vector
---

# WebGL persona caps and readPixels noise

## The caps are Intel and AMD values; do not match an NVIDIA reference

cuttle's Windows personas claim Intel and AMD Direct3D11 adapters only.[^pool]
For them `MAX_VERTEX_UNIFORM_VECTORS` is 4096 (and the matching component and
combined counts). `MAX_SAMPLES` is the one cap that differs between the tabled
D3D11 GPUs, 8 or 16 per device, with 8 for an untabled one.[^p16][^p58]

An NVIDIA machine reads 4095, 16380 and 212988 instead. That is ANGLE's
`skipVSConstantRegisterZero` workaround, which reserves one vertex uniform
vector on NVIDIA only.[^angle][^p16] A diff against an NVIDIA reference is
therefore expected, not a persona bug. Change the table only against a
reference from the same GPU vendor the persona claims.

## readPixels noise must be bounded by the validated destination

`readPixels` validates the destination for the requested format and type,
including pack skips and row padding. A 64x64 `ALPHA`/`UNSIGNED_BYTE` read
validates a 4096-byte view. Noise that assumed 4 bytes per pixel wrote
width*height*4 = 16384 bytes into it: an out-of-bounds write, reachable from
any page.[^p16]

The rule for any readback noise: compute the span from the same validation
(`ComputeImageSizeInBytes` with the pack parameters) and write only inside it,
row by row. Patch 0016 now noises only `RGBA`/`UNSIGNED_BYTE`, the format pixel
hashes read, over exactly that span. It skips transparent and flat pixels, so
a clear reads back exact, and never touches alpha; a WebGL2 read into a
`PIXEL_PACK` buffer is not noised.[^p16]

[^p16]: Patch 0016 - persona WebGL identity, caps table and readPixels noise
[^p58]: Patch 0058 - WebGL2 int64 caps from the same table
[^pool]: Windows persona GPU pool - Intel and AMD Direct3D11 adapters only
[^angle]: ANGLE D3D11 vertex uniform reservation
