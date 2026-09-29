---
type: Finding
title: Launch and X-server tells a headed container adds on top of the browser
description: In cuttle's headed Linux container, three page-visible tells came from the X server and the launch, not the patched binary - Xvfb resets when its last client leaves and drops an uploaded keymap unless run with -noreset; --no-sandbox without --test-type shows an infobar that adds 56px of window chrome; Chrome's Linux custom frame adds 4px per side where real Mac Chrome has none - and only a probe of the browser the daemon itself launches sees them.
tags: [stealth, docker, xvfb, keyboard, window, infobar, gates]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T18:35:00+00:00" }
sources:
  - id: measure
    resource: "Measurement on 2026-09-29: the stealth-Chromium 154 release binaries in per-persona test images, started through the image entrypoint, browser launched by cuttle serve, detect.py attached to serve's CDP endpoint; navigator.keyboard.getLayoutMap() and outerWidth/outerHeight minus innerWidth/innerHeight, before and after each fix"
    title: Daemon-level measurements of the 154 image
  - id: realref
    resource: "benches/realref.py on real Chrome 154.0.8037.58 on a Mac and on a Windows 11 PC (unlocked console), 2026-09-29"
    title: Real-Chrome 154 baselines
  - id: xserver
    resource: https://www.x.org/releases/current/doc/man/man1/Xserver.1.xhtml
    title: Xserver(1) - -noreset and server reset on last client disconnect
  - id: infobar
    resource: https://github.com/chromium/chromium/blob/9d414657c043ed805664bd3d0d9e3081ee3e13e2/chrome/browser/ui/startup/infobar_utils.cc
    title: infobar_utils.cc - startup infobars, including the bad-flags prompt, are skipped under --test-type or automation
  - id: entrypoint
    resource: /ops/docker/bin/docker-entrypoint.sh
    title: Image entrypoint (X server start, -noreset, keymap load)
  - id: keymap
    resource: /ops/docker/bin/xkb-persona-keymap.sh
    title: Per-persona IntlBackslash keymap, shared by the entrypoint and the gates
  - id: args
    resource: /packages/cuttle/internal/fingerprint/args.go
    title: getDefaultStealthArgs - --test-type next to --no-sandbox
  - id: pool
    resource: /packages/cuttle/internal/serve/pool.go
    title: Stock prefs seeding, including browser.custom_chrome_frame on the macOS persona
  - id: detect
    resource: /packages/browser/benches/detect.py
    title: detect.py - seeds serve's prefs and forces --fingerprint-webrtc-ip
---

# Launch and X-server tells a headed container adds on top of the browser

A stealth binary can be exact and the container still leak. On the 154
release, three tells a page can read came from how the image runs X and how
the browser is launched. Each was invisible to gates that launch their own
browser.[^measure]

## Xvfb drops an uploaded keymap unless it runs with -noreset

An X server started without `-noreset` resets when its last client
disconnects, and a reset reloads the default keymap.[^xserver] Straight after
the X server comes up, `xkbcomp` uploading the persona keymap is often the
only client. The upload succeeds, `xkbcomp` exits 0, the server resets, and
the keymap is gone. IntlBackslash stayed `<`, which neither real persona
reports.[^measure][^realref] Run Xvfb and Xvnc with
`-noreset`.[^entrypoint] The default us/pc105 map already matches real
Chrome in 47 of 48 keys, so `setxkbmap -model pc105 -layout us` changes
nothing; only IntlBackslash (`<LSGT>`) needs a per-persona override.[^keymap]

With `-noreset` and the per-persona remap, all 48 `getLayoutMap()` keys equal
real Chrome 154, in plain Xvfb and in VNC mode. IntlBackslash reads `\` on
Windows and the section sign (U+00A7) on macOS.[^measure][^realref]

## --no-sandbox needs --test-type, or an infobar shows

Headed Chrome started with `--no-sandbox` shows the "unsupported command-line
flag: --no-sandbox" infobar. Chrome skips its startup infobars, the bad-flags
prompt included, only when `--test-type` is set or automation is
enabled.[^infobar] The infobar is visible to a person on the viewer. A page
reads it as 56px of extra window chrome: outerHeight - innerHeight was 147.
`--test-type` belongs next to `--no-sandbox` on every launch, not only in one
entrypoint mode.[^args] With it, chromeHeight reads 91. Real Chrome 154 reads
94 on Windows and 87 on macOS.[^measure][^realref]

## Chrome's Linux custom frame is 4px per side

In the restored window state, Chrome's own Linux frame adds a 4px border per
side, so outerWidth - innerWidth reads 8 on both personas. Real Mac Chrome
reads 0.[^measure][^realref] The pref `browser.custom_chrome_frame=false`
switches to the system frame, which under the container's window manager
gives chromeWidth 0 and chromeHeight 87, both exactly real macOS. cuttle seeds
it on the macOS persona.[^pool] Real Windows reads chromeWidth 15, from its
invisible resize borders; no pref reaches that value.[^realref]

## Gates must measure the browser the daemon launches

Harness gates that start their own Chrome and profile miss all of the above.
They also miss the prefs the daemon seeds into a fresh profile
(canMakePayment basicCard, `<a ping>` link auditing, the hidden bookmark bar)
and its `--fingerprint-webrtc-ip`.[^measure] detect.py now seeds the prefs
`serve` writes and forces the WebRTC IP as the pool does, and the keymap
script is shared with the gates.[^detect][^keymap] The authoritative check
is still a daemon-level probe: start the image through its entrypoint, let
`cuttle serve` launch the browser, and attach the detector to serve's CDP
endpoint.

[^measure]: Daemon-level measurements of the 154 image
[^realref]: Real-Chrome 154 baselines
[^xserver]: Xserver(1) - -noreset and server reset on last client disconnect
[^infobar]: infobar_utils.cc - startup infobars skipped under --test-type or automation
[^entrypoint]: Image entrypoint (X server start, -noreset, keymap load)
[^keymap]: Per-persona IntlBackslash keymap, shared by the entrypoint and the gates
[^args]: getDefaultStealthArgs - --test-type next to --no-sandbox
[^pool]: Stock prefs seeding, including browser.custom_chrome_frame on the macOS persona
[^detect]: detect.py - seeds serve's prefs and forces --fingerprint-webrtc-ip
