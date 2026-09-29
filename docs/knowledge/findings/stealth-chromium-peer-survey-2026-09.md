---
type: Finding
title: Stealth-Chromium peer survey, September 2026 - where our series lags
description: Snapshot of a 2026-09-29 survey of clark-browser, CloakBrowser, ChromiumFish, fingerprint-chromium, apostate and Brave farbling against our 154 series, plus fresh real-Chrome 154 baselines; ranks the five tells several sources confirm (unseeded canvas noise, the WebGL shader-language leak, software-rendering WebGL/WebGPU capabilities, a no-op WebRTC IP switch, page-visible CDP) and six cheap fixes.
tags: [stealth, fingerprint, patches, webgl, canvas, webrtc, cdp, survey]
status: stable
stale_after: "2027-01-01T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T15:35:00+00:00" }
sources:
  - id: clark
    resource: https://github.com/clark-labs-inc/clark-browser
    title: clark-browser (MIT) - last push 2026-06-22, nothing past stealth5
  - id: cloak
    resource: https://github.com/CloakHQ/CloakBrowser
    title: CloakBrowser - wrappers MIT, C++ patches proprietary (BINARY-LICENSE forbids reverse engineering)
  - id: fish
    resource: https://github.com/arman-bd/chromiumfish
    title: ChromiumFish (MIT) - 28 patches on Chromium 151
  - id: adryfish
    resource: https://github.com/adryfish/fingerprint-chromium
    title: fingerprint-chromium (BSD-3) - 18 fingerprint patches incl. 30 captured GPU capability configs
  - id: apostate
    resource: https://github.com/heretic-tech/apostate
    title: apostate (GPL-3.0) - 153 patches on Chromium 152, docs/known-gaps.mdx
  - id: clearcote
    resource: https://github.com/clearcotelabs/clearcote-browser
    title: clearcote open series (BSD-3) - 100-webrtc-leak, 110-runtime-enable, 060-canvas
  - id: brave
    resource: https://github.com/brave/brave-core/blob/master/third_party/blink/renderer/core/farbling/brave_session_cache.cc
    title: Brave farbling (MPL-2.0) - content-keyed canvas perturbation
  - id: ug3663
    resource: https://github.com/ungoogled-software/ungoogled-chromium/issues/3663
    title: ungoogled-chromium #3663 - canvas noise hash changes per reload, closed not planned
  - id: dabi
    resource: https://deviceandbrowserinfo.com/are_you_a_bot
    title: deviceandbrowserinfo are_you_a_bot test documentation
  - id: probe
    resource: "probe of a running cuttle 151 container (macOS persona) on 2026-09-29 (session record): toDataURL/getImageData hashes differ per read, 0 ICE candidates, getTranslatedShaderSource returns non-MSL, MAX_TEXTURE_SIZE 8192, Xvfb keyboard layout map"
    title: Probe of our own build
  - id: realref
    resource: "benches/realref.py runs on 2026-09-29 (session record): real Chrome 154.0.8037.58 on a Mac and on a Windows 11 PC"
    title: Real-Chrome 154 baselines
---

# Stealth-Chromium peer survey, September 2026

A point-in-time survey; re-run it rather than trusting it after `stale_after`.

## Upstream status

- clark-browser, the series we forked, is dormant; every fix it made to a patch
  we share is already in ours.[^clark]
- CloakBrowser has never published its C++ patches and licenses them as
  proprietary, so it is an idea source only.[^cloak]
- Portable code (with the notice kept): ChromiumFish (MIT),[^fish]
  fingerprint-chromium (BSD-3),[^adryfish] clearcote's open series
  (BSD-3).[^clearcote] Ideas only: apostate (GPL-3.0)[^apostate] and Brave
  farbling (MPL-2.0).[^brave]
- ungoogled-chromium closed its fingerprinting requests as not planned.[^ug3663]

## Measuring a real Windows baseline

A first real Chrome 154 capture on Windows, run over plain ssh, landed in the
non-interactive session 0. It saw a 1024x768 screen with no taskbar, and
are_you_a_bot flagged it (`hasInconsistentWorkerValues`). Rerun in the logged-in
console session through a scheduled task, the same machine read 2560x1440 at
DPR 1.5 with a 48px taskbar, and are_you_a_bot reported `isBot: false`.[^realref]
The flag came from how the measurement was taken, not from detector drift in
154, and the 151 -> 154 worker code paths are unchanged. A real-Windows baseline
must run in the interactive console session, never over ssh. A locked console
still zeroes the outer window size and reports pointer/hover `none`, so capture
unlocked. Real Chrome 154 on a Mac matched its 151 baseline except the UA
major.

## Five tells confirmed by several sources

1. **Canvas, client-rect and measureText noise is unseeded.** ungoogled's noise
   switches draw `base::RandDouble()` per read or per Document, so one seed's
   canvas hashes differently on every read and reload.[^probe][^ug3663] That
   instability is what CreepJS reports as lies. Fix: noise keyed on seed, site
   and pixel content, skipping flat-colour neighbourhoods and alpha - per-seed
   distinct, per-read stable.[^brave][^apostate]
2. **WebGL shader-language leak.** `WEBGL_debug_shaders.getTranslatedShaderSource`
   returns Linux ANGLE output under a Metal or D3D11 renderer string; that
   mismatch is exactly what `hasInconsistentWebGLShaderLang` checks.[^dabi][^probe]
   It is fixable at that one call (emit the persona's dialect), not only by a
   backend swap. Dropping the extension is worse: real Chrome exposes it.[^fish]
3. **WebGL/WebGPU capabilities read as SwiftShader** (`MAX_TEXTURE_SIZE` 8192,
   software precision formats, ASTC/ETC extensions, a CPU WebGPU adapter); our
   patches spoof strings only. Captured per-GPU tables exist under
   BSD-3.[^adryfish][^fish]
4. **`--fingerprint-webrtc-ip` is read by no patch** and the build emits 0 ICE
   candidates; `enable_mdns=false` also removes real Chrome's `.local` host
   candidates.[^probe][^clark] A public-candidate rewrite exists under
   BSD-3.[^clearcote]
5. **CDP is page-visible** (Runtime.enable side effects, bindings, a probeable
   localhost debug port), and `detect.py` cannot see it because it never
   enables Runtime while playwright does.[^cloak][^clearcote][^apostate]

## Cheap fixes (small effort each)

6. Xvfb's keyboard layout map is an impossible combination; set a real one.[^probe]
7. Audio: a soundless container runs at 44.1 kHz (`baseLatency` 0.010884, which
   no Windows machine reports); noise on every AudioBuffer read likely causes
   CreepJS's AudioBuffer lie - move it to rendered output.[^apostate][^fish]
8. `jsHeapSizeLimit` follows the host, not the persona's `deviceMemory`.[^apostate]
9. Persona CSS: Windows `ActiveText` resolves red (fires `hasKnownBgColor`); a
   Mac persona should report p3/HDR/30-bit.[^probe][^fish]
10. A discrete GPU claimed over SwiftShader raised anti-detect scores in
    ChromiumFish's measurements; prefer integrated GPUs in the Windows pool.[^fish]
11. Verify whether patch 0040's minimal/no-cross-origin referrer features are
    neutralised at launch; stock Chrome sends a cross-origin Referer.[^fish][^clark]

## Tooling worth adopting

`git apply` refuses zero-context hunks (closing the `-F0` hole), and a
round-trip gate (apply the regenerated series to a pristine tree, require zero
diff) catches silently dropped hunks.[^fish]

## Rejected

Proprietary or GPL code; a remote real-Windows canvas renderer (identical
pixels across seeds); request-validation headers keyed off Chrome's embedded
API key; socket TTL changes (sites see the proxy exit); a CRT math port
reverse-engineered from a Windows DLL.[^fish]

[^clark]: clark-browser (MIT) - last push 2026-06-22, nothing past stealth5
[^cloak]: CloakBrowser - wrappers MIT, C++ patches proprietary
[^fish]: ChromiumFish (MIT) - 28 patches on Chromium 151
[^adryfish]: fingerprint-chromium (BSD-3)
[^apostate]: apostate (GPL-3.0)
[^clearcote]: clearcote open series (BSD-3)
[^brave]: Brave farbling (MPL-2.0)
[^ug3663]: ungoogled-chromium #3663
[^dabi]: deviceandbrowserinfo are_you_a_bot test documentation
[^probe]: Probe of our own build
[^realref]: Real-Chrome 154 baselines
