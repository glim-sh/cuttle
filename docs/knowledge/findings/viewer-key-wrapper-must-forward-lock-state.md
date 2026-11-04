---
type: Finding
title: A noVNC key wrapper that drops the lock-state arguments taps CapsLock and NumLock before every key
description: noVNC 1.5's keyboard calls onkeyevent(keysym, code, down, numlock, capslock); a wrapper that forwards only the first three makes the lock states undefined, which noVNC's lock sync reads as a known mismatch once the server has reported its LEDs, so every keydown reached the remote page preceded by a trusted CapsLock and NumLock tap - a behavioural fingerprint - until the viewer forwarded every argument.
tags: [viewer, novnc, kasmvnc, keyboard, stealth, humanize]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-04T21:57:00+00:00" }
sources:
  - id: keyboard
    resource: https://github.com/novnc/noVNC/blob/v1.5.0/core/input/keyboard.js
    title: noVNC 1.5.0 keyboard.js - _sendKeyEvent(keysym, code, down, numlock = null, capslock = null) calls onkeyevent with all five
  - id: rfb
    resource: https://github.com/novnc/noVNC/blob/v1.5.0/core/rfb.js
    title: noVNC 1.5.0 rfb.js - _handleKeyEvent's lock sync (`!== null` checks) and _handleLedEvent
  - id: measure
    resource: "Measurement on 2026-11-04 against a cuttle 0.17.1 container with CUTTLE_VNC=1: 30 presses of x through the shipped viewer from a headless Chrome 156, keydowns recorded on a page in the remote browser, then the same with all five arguments forwarded"
    title: Remote-page keydown counts and key-to-frame latency, shipped vs fixed viewer
  - id: viewer
    resource: /ops/docker/bin/vnc-viewer.html
    title: The viewer page and its Cmd-swallowing onkeyevent wrapper
  - id: smoke
    resource: /packages/cuttle/test/smoke/viewer.go
    title: viewer-lock-keys smoke check
---

# Finding

noVNC's `Keyboard` reports each key as `onkeyevent(keysym, code, down,
numlock, capslock)`, where the last two are the local lock states (`null`
when unknown, e.g. NumLock on macOS).[^keyboard] `RFB._handleKeyEvent` keeps
the server's lock state from the LED pseudo-encoding and, on a keydown whose
local state is not `null` and differs from it, sends a CapsLock or NumLock tap
first and forgets the remote state until the server reports it again.[^rfb]

The viewer wraps `onkeyevent` to swallow macOS Cmd.[^viewer] Through 0.17.2
the wrapper forwarded only `(keysym, code, down)`. The two dropped arguments
arrive as `undefined`, which passes the `!== null` checks and never equals a
boolean, so once KasmVNC reported its LEDs every keydown was preceded by a
CapsLock tap and a NumLock tap, the server's lock state flipped on each one,
and each LED echo re-armed the next tap. Measured on the remote page, 30
presses of `x` arrived as 30 CapsLock, 30 NumLock and 15 Shift keydowns
around the 30 `x` keydowns, all `isTrusted` - a lock-state pattern no person
produces, readable by any page.[^measure] Forwarding every argument left the
30 `x` keydowns plus at most one CapsLock resync when the remote lock was
genuinely out of step, and cut key-to-frame p50 by about 13-16 ms.[^measure]

Any wrapper around a noVNC callback must forward its arguments with rest
parameters rather than a hand-copied list; the `viewer-lock-keys` smoke check
types through the viewer and fails if lock taps reach the socket.[^smoke]

With the taps gone, the first use of a character the persona keymap lacks can
drop: KasmVNC binds a spare keycode for it and the remote Chrome races the
keymap reload. The taps' extra round trips had been hiding that race.[^measure]

[^keyboard]: noVNC 1.5.0 keyboard.js
[^rfb]: noVNC 1.5.0 rfb.js
[^viewer]: The viewer page and its Cmd-swallowing onkeyevent wrapper
[^measure]: Remote-page keydown counts and key-to-frame latency, shipped vs fixed viewer
[^smoke]: viewer-lock-keys smoke check
