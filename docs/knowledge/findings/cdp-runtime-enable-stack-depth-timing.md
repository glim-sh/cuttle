---
type: Finding
title: Runtime.enable makes every new Error cost more with call depth, and pages time it
description: CDP Runtime.enable, which every driver sends, raises V8's stack capture depth to 200 frames, so the cost of new Error grows with call depth instead of stopping at Error.stackTraceLimit; bot detectors time that (hasInconsistentTimingResolution) and flag even real Chrome 154 with Runtime on, on every load; patch 0066 captures one frame instead.
tags: [stealth, cdp, v8, timing, detection]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T20:49:00+00:00" }
sources:
  - id: patch
    resource: /packages/browser/patches/0066-cdp-runtime-stack-capture-depth.patch
    title: Patch 0066 - Runtime.enable captures one frame
  - id: v8
    resource: "chromium 154.0.8037.57, v8 671f7c27ac04: src/inspector/v8-runtime-agent-impl.cc (enable() sets V8StackTraceImpl::kDefaultMaxCallStackSizeToCapture, 200)"
    title: V8 inspector runtime agent
  - id: detect
    resource: /packages/browser/benches/detect.py
    title: detect.py - are_you_a_bot loaded repeatedly with Runtime on
  - id: dabi
    resource: https://deviceandbrowserinfo.com/are_you_a_bot
    title: deviceandbrowserinfo are_you_a_bot
---

# Runtime.enable makes every new Error cost more with call depth

A browser can be spotless and still read as driven, because the driver's
own CDP session changes how V8 behaves.

## The mechanism

`Runtime.enable` sets V8's capture depth to
`kDefaultMaxCallStackSizeToCapture`, 200 frames.[^v8] Every new Error then
walks max(Error.stackTraceLimit, 200) frames, and console calls capture up to
200 frames instead of one. With no Runtime session the walk stops at
`Error.stackTraceLimit` (10), so a throw costs the same at any call depth; with
one, the cost grows linearly up to depth 200.[^patch]

## How it is detected

A page times `new Error` at depths 10..200 and fits a line: r2 is near 0 on a
plain Chrome and near 0.9 once Runtime is on. are_you_a_bot reports it as
`hasInconsistentTimingResolution`.[^dabi][^patch] Real, unpatched Chrome 154
with only Runtime.enable is flagged on every load (10 of 10), so this is a
driver tell, not a fingerprint tell: no spoofed surface fixes it, only the
capture depth does.[^patch]
The fit is noisy, so one clean load proves nothing; a gate must load the page
repeatedly with Runtime on and compare the flag rate with a plain Chrome's.[^detect]

## The fix and its cost

Patch 0066 makes `Runtime.enable` request one frame, the depth the console
captures with no session. The page still gets its own
`Error.stackTraceLimit` frames in `error.stack`, so the cost is flat again. A
driver's console and exception events carry one frame instead of up to 200;
it can still raise the depth with `Runtime.setMaxCallStackSizeToCapture`,
which the patch leaves alone.[^patch]

[^patch]: Patch 0066 - Runtime.enable captures one frame
[^v8]: V8 inspector runtime agent
[^detect]: detect.py - are_you_a_bot loaded repeatedly with Runtime on
[^dabi]: deviceandbrowserinfo are_you_a_bot
