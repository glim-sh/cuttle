---
type: Decision
title: Selkies is the takeover viewer
description: The viewer a person uses to take over the browser (CUTTLE_VNC=1, port 6080) is Selkies 2.2.1 streaming H.264 over one WebSocket from the same Xvfb display plain mode uses, replacing KasmVNC and a hand-written noVNC page; it was chosen for its phone client and a keyboard path that delivers characters the persona lacks on first use, and every setting that could move the fingerprint or reach a third party is pinned off.
tags: [viewer, selkies, kasmvnc, keyboard, stealth, docker, mobile]
status: stable
generated: { by: codex/gpt-6, at: "2026-11-04T23:27:11Z" }
sources:
  - id: phone
    resource: /packages/cuttle/internal/serve/viewerfocus.go
    title: Isolated-world metadata sampler and loopback stream
  - id: overlay
    resource: /ops/docker/selkies/keyboard.js
    title: Phone keyboard prediction and Selkies text-diff input
  - id: compare
    resource: "Viewer comparison on 2026-11-04: Selkies 2.2.1, KasmVNC 1.3.3 with cuttle's noVNC 1.5.0 page, neko 3.2.0, MediaMTX v1.21.3 WebRTC and two WebTransport prototypes, each against a cuttle container's display on an Apple Silicon laptop (5 vCPU VM, several runs sharing it), driven from Chrome 156 and the iOS 27.2 Simulator"
    title: Viewer candidate measurements
  - id: measure
    resource: "Measurement on 2026-11-04 of this change's arm64 image: the smoke harness's takeover checks (six runs), ten non-persona characters typed at 0, 10 and 30 ms gaps after a keymap reset, and the daemon gate in VNC modes"
    title: Selkies image measurements
  - id: lockfix
    resource: https://github.com/glim-sh/cuttle/pull/151
    title: "glim-sh/cuttle#151 - the noVNC page's lock-key injection, fixed before this switch"
  - id: config
    resource: https://docs.linuxserver.io/selkies/user-guide/configuration/
    title: Selkies configuration reference (settings and defaults)
  - id: input
    resource: https://github.com/selkies-project/selkies/blob/2.2.1/src/selkies/input_handler.py
    title: Selkies 2.2.1 input_handler.py - spare-keycode overlay and its 25 ms bind settle
  - id: entrypoint
    resource: /ops/docker/bin/docker-entrypoint.sh
    title: Image entrypoint - Xvfb, display padding, the Selkies flags
  - id: dockerfile
    resource: /ops/docker/Dockerfile
    title: Dockerfile - the pinned Selkies .deb per arch
  - id: smoke
    resource: /packages/cuttle/test/smoke/viewer.go
    title: Smoke viewer checks - streaming through root and prefixed HTTPS proxies, and a phone-sized takeover
---

# Decision

The takeover viewer is [Selkies](https://github.com/selkies-project/selkies)
2.2.1, installed from the project's Debian trixie `.deb` per arch and pinned
by sha256.[^dockerfile] With `CUTTLE_VNC=1` the entrypoint runs it against
`:99` on `CUTTLE_VNC_PORT` (6080), restarting it if it exits; port, env and
viewer link are unchanged, so the chart, the CLI and the loopback tunnels keep
working.[^entrypoint] An ingress must now be HTTPS and strip a path prefix from
the socket too (below). The display is the same Xvfb plain mode runs, so a
container with the viewer on renders exactly like one without it. KasmVNC's
X server and its second viewer endpoint are gone, and so is the noVNC page.

## Why Selkies

On latency the two TCP-only finalists tied: KasmVNC through cuttle's page
measured 65-84 ms key-to-frame p50 (52-68 ms once its lock-key injection was
fixed[^lockfix]), Selkies 74 ms (p90 133 ms, 90 ms behind an HTTPS proxy), both
over one WebSocket that a TCP-only ingress carries.[^compare] neko reset the
persona keymap on takeover; MediaMTX WebRTC needs a media port and ran 150-270
ms click-to-frame; WebTransport needs UDP and stalled in iOS Safari unless run
as one long-lived stream.[^compare] What separated the two was the client and
the keyboard:

- **Phone.** Selkies ships touch and trackpad modes, an on-screen keyboard and
  Safari clipboard; the KasmVNC page had none and no pinch-zoom on iOS.[^compare]
- **First use of a missing character.** Both bind a spare keycode for a
  character the persona keymap lacks, and a browser that reads the key before
  it has refetched the keymap drops it. Through KasmVNC the first `ж` was lost
  and `привіт` landed as `пріт`.[^compare] Selkies waits 25 ms after a bind
  before pressing the key.[^input] Ten such characters (Cyrillic, ü, é, ß)
  typed once after a keymap reset landed complete every time at 0, 10 and 30 ms
  between keys, and the smoke check types them once and requires the whole
  string.[^measure][^smoke]
- **No lock-key taps.** Selkies sends no CapsLock or NumLock around typed
  keys; a page recorded exactly the typed keys, Shift and Ctrl+V, all
  `isTrusted`.[^measure]

The cost: about 135 MB more image on disk (40 MB compressed) on arm64, about
0.5 core while the screen moves at 1080p and 7-15% of one when it is still,
1.1-1.4 Mbit/s for an animated region.[^measure][^compare] And the page runs
only in a secure context, below.

## The settings, and why

Every one is a flag or `SELKIES_*` variable in the entrypoint.[^entrypoint]
Defaults are from the configuration reference.[^config]

- `--enable-resize=false --second-screen=false`: the client asks to resize the
  display to its window by default, which would put a phone's shape under
  the page. Measured with a 390x844 viewer: screen 1710x1107, window
  1710x1012, both as before it connected.[^measure]
- `--mode=websockets --enable-dual-mode=false`: dual mode lets the client switch
  to WebRTC, whose default STUN and TURN servers are public third parties
  (a Google STUN host and a public TURN relay) - traffic from the container
  that is no part of the browser's persona.
- Audio, microphone, webcam, gamepad, printing, sharing links and file
  transfers off, and their sidebar panels hidden: none serve a takeover, and
  each is a path into or out of the session.
- `--encoder=h264enc --rate-control-mode=crf --use-paint-over-quality=true
  --video-streaming-mode=false`: constant quality, sharpened once the screen is
  still; "Turbo" streaming, on by default, kept encoding a static screen and
  cost about a third of a core idle.[^compare]
- `--enable-https=false --enable-basic-auth=false --public`: plain HTTP on
  every interface inside the container, with the loopback port mapping as the
  boundary, as for CDP. The websocket stays same-origin only (the default), so
  a proxy must keep the browser's `Host` header. Whoever reaches the port also
  reaches Selkies' `/api/screenshot`, `/api/recording` (an MP4 of the session
  written inside the container) and `/api/sessions` (list and kick viewers),
  which KasmVNC's `-DisableBasicAuth` had shut. That reach already means full
  control of the browser through the socket, so it changes no boundary; the
  one switch that shuts them, `--master-token`, also refuses the page's own
  socket until each page is handed a provisioned token, which the CLI's plain
  viewer link cannot carry.
- Clipboard on, seamless (the defaults): Ctrl+V in the viewer pastes the
  person's local clipboard, which the smoke check exercises. Once the person
  grants clipboard access, Selkies also reads the local clipboard whenever the
  viewer tab gains focus and copies it into the session, where the page and an
  agent can read it - wider than the old page, which sent it only on a paste.
  `SELKIES_CLIPBOARD_SEAMLESS=false` would confine it to the sidebar's
  clipboard box, at the cost of the paste a sign-in needs.
- `--scaling-dpi=96`: the client asks for its own device pixel ratio as the
  session DPI; held at 96 so a phone cannot change it should the image ever
  ship `xrdb`, which applies it.
- `SELKIES_RUN_AFTER_DISCONNECT` re-applies the persona keymap: the spare
  keycodes show in `navigator.keyboard.getLayoutMap()` while a person is
  connected (2 of 48 entries differed after typing those ten characters), and
  the map read 48 of 48 persona keys again once the viewer left.[^measure] A
  crash skips that hook, so the entrypoint's restart loop re-applies it too.

Selkies grows the X screen to a width that is a multiple of 8, which on a
1710-wide MacBook Air screen made the maximized window 1712 wide. The
entrypoint pads the display itself and gives openbox the padding as a right
margin, so the window keeps the screen's width.[^entrypoint]

## Secure context

Selkies decodes with WebCodecs and shows only an error outside a secure
context.[^measure] Loopback is one, so `cuttle up` and the ssh and k8s tunnels
work as before; anything reached by another name needs an HTTPS proxy, and the
`direct` backend's `vnc_url` keeps its `https://` link.

[^compare]: Viewer candidate measurements
[^measure]: Selkies image measurements
[^lockfix]: glim-sh/cuttle#151
[^config]: Selkies configuration reference
[^input]: Selkies 2.2.1 input_handler.py
[^entrypoint]: Image entrypoint
[^dockerfile]: Dockerfile - the pinned Selkies .deb per arch
[^smoke]: Smoke viewer checks

## Phone focus signal

The daemon samples editable-field metadata in a named isolated world. It adds
no main-world binding, property or listener, and never reads values. Each sample
resolves the current frame context again, so navigation cannot reuse an old
context ID. Same-origin iframe fields are clipped to their containing frame;
cross-origin and transformed frames use the manual keyboard fallback. A drag or two-finger
gesture on the video marks cached fields stale until the next report, so a tap
in between waits for the focus report. A stale or ambiguous-focus stream releases the local keyboard.

Only each window's foreground tab is sampled, chosen by the tab target's
`tabActive` (CDP `Target.getTargets` with a `tab` filter), not by the page's
`visibilityState` or `hasFocus()`: a driver's focus emulation (Playwright's
attach, and the daemon's own per-page pin) captures every tab it attaches to,
so each one reports visible and focused. `document.activeElement` stays real
under emulation, so the focused field inside the chosen tab is still correct.
Two foreground tabs that both claim focus (two windows under emulation) are
still refused.

The viewer server forwards one same-origin SSE path to the loopback-only daemon
route. This keeps existing ingress and port mappings intact and lets a future
viewer proxy carry the route under its own prefix. The patcher asserts its
upstream insertion anchors and retains the original notices. Browser geometry
comes from CDP, because persona screen coordinates and DPR are intentionally
synthetic.[^phone] Typed text still uses Selkies keysyms and its X input implementation;
no page input injection, lock-key toggles, or remote resize is added.[^overlay]

Passwords disable local credential AutoFill because the viewer has a different
origin from the page. OTP suggestions retain `one-time-code`. The manual
keyboard remains available. Physical iPhone keyboard visibility is unverified;
the executable acceptance fixture is
[phone-keyboard.mjs](../../../packages/cuttle/test/smoke/phone-keyboard.mjs).

[^phone]: Isolated-world sampler in `packages/cuttle/internal/serve/viewerfocus.go` and its embedded script.
[^overlay]: Phone keyboard overlay in `ops/docker/selkies/keyboard.js`.
