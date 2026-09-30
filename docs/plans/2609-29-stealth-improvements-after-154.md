# Stealth improvements after the 154 rebase

Date: 2026-09-29. Scope: the 11 items in [the peer survey](../knowledge/findings/stealth-chromium-peer-survey-2026-09.md), plus the rebuild-deferred items folded in from issues [#45](https://github.com/glim-sh/cuttle/issues/45), [#50](https://github.com/glim-sh/cuttle/issues/50), [#53](https://github.com/glim-sh/cuttle/issues/53) and [#80](https://github.com/glim-sh/cuttle/issues/80). Target, superseded by the decisions below: originally a separate `browser-v154.0.8037.57-2`.

## Decisions after drafting (2026-09-29)

These override the sections below wherever they conflict.

- **Fold into the 154 release.** Nothing ships as a pure rebase. The first 154 build (patched with the rebased series only) runs to completion, including the x64 smoke. That proves the rebase alone compiles, links and runs, and leaves a fallback binary. It is not published, and detector gates are not run on it, because its navigator.userAgent still says 151. If arm64 lags far behind when integration is ready, it is switched to the merged series mid-build. All lanes then land as one incremental rebuild of both targets on the same box tree, and that build is published as the 154 release.
- **Lanes prepare now, in parallel.** Each lane has its own branch off `feat/chromium-154-rebase`: t, m, a, canvas, webgl, webrtc, cdp, audio, display, font, net. The box is read-only for the lanes until the first 154 build finishes; the coordinator merges and runs prep.
- **Integration is all at once, with a straggler cutoff (this supersedes the per-cycle train below).**
  - When the first 154 builds finish, merge every lane that is done, run one prep, and build both targets with `ninja -k 0`, so every compile error surfaces in one pass. Fix in place and rerun: only the failed and dependent edges rebuild.
  - A lane that is not done by then goes in as a second incremental pass, so a straggler never holds the box idle.
  - The first arm64 build runs the pre-retry script. A box-side watcher (out-of-repo tooling) reruns its build stage once if it exits red, so the known devtools ordering race costs no idle time.
- **Stock prefs:** `<a ping>` (`enable_a_ping`) is restored to Chrome's default; the rest of ungoogled's defaults stay, except those listed under lane A.
- **Lane A owns every args.go edit.** The rects-noise flag, `--disable-features`, the Windows pool and the dead-switch emission all live there. Other lanes describe the args.go changes they need.
- **Windows pool:** integrated GPUs only, 16 GB or more, restricted to device IDs with captured BSD-3 tables in adryfish: Intel 9A49, 3EA0, 46A6, A7A0, 9B41, and AMD 1638. The webgl lane keys its capability tables on the renderer strings lane A emits.
- **Shader dialect (item 2):** the webgl lane owns the spike (extracting the dabi rule and capturing real output) and option B. Lane M skips spike (i).
- **Audio 7a:** implement in C++ as 0061 directly (48000 Hz; baseLatency from real Chrome), not gated on the ALSA spike.
- **system-ui (item b), design:**
  - Inter (OFL), re-widthed to SF Pro's advance widths (Text optical size, one face per weight) by the existing metrics pipeline.
  - Installed under an internal family name.
  - Patch 0063 maps `system-ui` to it on the macOS persona.
  - A direct lookup of the internal name resolves as absent (CSS, canvas `font`, FontFace/`document.fonts.check`, `local()`), and "SF Pro" stays uninstalled, as on a real Mac.
  - Separate lane `font`.
  - The Helvetica mapping in item (b) below is dropped.
- **152-154 web-platform review:** a separate review of what 152-154 changed on the web platform for fingerprinting. Its must-fix items join the release: navigator.userAgent hardcoded to 151 in 0006, and the new navigator.cpuPerformance reporting the host (lane `ua`). The Windows `hasInconsistentWorkerValues` flag came from a baseline taken over ssh in session 0, not from 154.
- **Tooling:** `git apply` with the round-trip gate (lane T) ships in this release.
- **kache:** not adopted. Read `sccache --show-stats` after each build and revisit only if the hit rate is poor on a version bump.
- **Release PR:** the body states that it addresses [#24](https://github.com/glim-sh/cuttle/issues/24), without an auto-close keyword.
- **Windows system-ui (lane `segoe`):** the pack's "Segoe UI" is Carlito renamed and 8.5% narrow at 16px. Rebuild it from Selawik (OFL, pinned), re-widthed to real Segoe UI metrics extracted on the Windows PC (metrics only, never the font files). 0063 maps Windows system-ui to it. It ships in this release.
- **Follow-up PR:** delete the unused Hetzner volume scripts (`packages/browser/hetzner/` provision.sh, teardown.sh, cloud-init.yaml); the README documents the snapshot flow.

## Lane results (2026-09-29)

Every lane round-trips its patches against the 154 box tree. The CDP lane also compile-checked its files with the box clang and the real x64 flags. An integration lane merges the branches into `feat/stealth-154-integration` (draft PR [#124](https://github.com/glim-sh/cuttle/pull/124) tracks the release) and compile-checks every changed file the same way before prep. Status: all 13 lanes are merged (36 patches, 0002 and 0045 deleted, integration head 36af8d3, merged into the PR branch). The series applies 36/36 with `git apply` onto pristine box sources, every changed .cc is syntax-clean with the real x64 flags, and no patch needed a fix.

| Lane | Patches / files | Result and corrections to this plan |
|---|---|---|
| t | build-linux.sh stage 4, `regen-patch.sh`, `just patch-lint` | `git apply` with `.browser-applied/` records reverses only changed and removed patches. A one-time seed came from the 9220dcc series (that migration is since removed). `git apply` does NOT reject zero-context insertions (they land at EOF), so stage 4 lints every patch itself. Nested v8/webrtc gitlinks work from `src/`. |
| m | `benches/probes.py`, realref.py, detect.py | Shared probes. Real Mac and Windows 154 baselines are kept out of the repo. The first Windows baseline was taken over ssh (session 0) and is invalid. ReportingObserver matches real, so `enable_reporting` stays. An ALSA null sink cannot move the sample rate; a PulseAudio null sink can (unused, see 0061). |
| a | args.go, pool.go, entrypoint | Windows pool of 8 integrated-GPU rows, 16 GB or more. Stock prefs written only when absent: canMakePayment, password manager, auto sign-in, card autofill, bookmark bar hidden, `enable_a_ping`. The rects-noise flag and `--disable-features` are dropped; the latter ships only with net's 0040/0019 change. The keyboard fix is an `<LSGT>` (IntlBackslash) remap per arch; setxkbmap pc105/us was a no-op. |
| net | 0040, 0019, 000-shared, 0050 | Stock referrer and client-hint defaults are in the binary. The dead switches kFingerprintLocation and kFingerprintAudioSampleRate are removed. 0002/0045 are deleted: headless-only, never compiled into chrome. |
| ua | 0006, new 0065 | The navigator.userAgent major comes from `--fingerprint-brand-version`, with no literal. cpuPerformance uses the persona tier via upstream `GetTierFromCpuInfo` (Apple M rule on macOS). |
| canvas | new 0055 | Seed- and content-keyed noise; toBlob/convertToBlob are coherent with toDataURL. Fixes Bromite's negative-origin write and backing-store mutation. Residual: a sub-rect read is noised differently from a full read. Real Windows canvas is itself unstable across reads. |
| webgl | 0016, new 0058-0060 | Per-persona caps, precision and hide-only extensions (real-Mac and adryfish D3D11 tables). The dabi shader dialect is spoofed for known shaders. A CPU WebGPU adapter resolves null; getPreferredCanvasFormat is bgra8unorm. The survey was wrong on ASTC/ETC: real Mac exposes them. The old allowlist hid 19 WebGL2 extensions. |
| webrtc | new 0057, pool.go | A `.local` host plus an srflx at the exit IP; nothing sent. TCP and relay are off in forced mode. Direct seeds get their egress IP. A caller-pinned policy suppresses the derived IP. |
| cdp | new 0056 (value-mirror.cc, injected-script.cc) | Console previews no longer fire page getters. This also fixes RegExp flags, NodeList length and console.table columns, which #50 did not list. |
| audio | 0026 rewritten, new 0061 | Multiplicative gain on offline output only. Persona 48 kHz with real per-hint latency. renderQuantumSize is already 128 (no patch). Windows `playback` rounds to the device buffer (960 frames), as real Windows does. |
| display | new 0062, 0064; 0011, 0013, 0054; `MenuBarHeight()` in cuttle_seed | Windows and Mac system colours; Windows menu/small-caption/status-bar in Segoe UI 12px. macOS colorDepth 30, P3, HDR. availTop and screenY are the menu bar (30 non-notched, 38 notched); event screen coordinates are consistent. Heap limit 4395630592 (the V8 cap applies from 8 GiB of host RAM, not 16 GB). |
| segoe | 0063 extension, Dockerfile Windows font stage, winfonts/metrics.json | Done (122ca3a, a9a59ad, 2baf58d). "Segoe UI" is Selawik 1.01 (pinned) stamped with real Segoe UI 5.72 metrics; italics are Carlito re-widthed. Weight 400 measures exactly like real Chrome 154 (13/16/72px), 700 is +0.12%. Windows system-ui maps to it via 0063, untested until the 154 rebuild. Gaps: no kerning (kerning-heavy strings 6-8% wide), 348 codepoints so Cyrillic falls back about 6% narrow. |
| kern | extract-font-metrics.py, rename-fonts.py, winfonts/metrics.json | Done (8ddc91d, d61a78e). Real Segoe UI's default Latin `kern` (GPOS pairs, legacy `kern` for the italics) flattened to integer codepoint pairs limited to the stand-in faces' coverage, 22,856 pairs, stamped as one GPOS kern lookup. Every measured width (pangram and "AVAWAY To Ta Te Yo LT", 400/700/italics, 13/16/72px) equals real Windows Chrome 154 exactly. |
| font | new 0063, Dockerfile font stage | macOS system-ui and BlinkMacSystemFont resolve to Inter re-widthed to SF Pro Text plus tracking (within 0.05% of real). The hidden name is absent by name. The `-apple-system` pin to Helvetica is removed, because real Chrome ignores `-apple-system`. |

Known limits of this release:

- **Selawik coverage.** Selawik covers 348 codepoints, so Cyrillic in "Segoe UI" falls back to another pack font and measures about 6% narrow.
- **Carlito italics.** The Italic and Bold Italic faces are Carlito re-widthed and keep its `liga`/`calt`/`dlig`, which real Segoe UI Italic may not form. Segoe UI Light, Semibold and Semilight are not shipped.
- **Fonts detectors probe that the packs lack.** Windows: Franklin Gothic, MS UI Gothic, Marlett, Segoe UI Light, Cambria Math, Gadugi, Myanmar Text, Nirmala UI, Javanese Text, Segoe MDL2 Assets, Bahnschrift, Ink Free and Segoe Fluent Icons (without Cambria Math the generic `math` width is wrong too). macOS: PingFang HK Light.
- **Apple Chancery (macOS `cursive`) is 1% wide.** Its AAT kerning and ligatures are not reproduced.
- **Font full and PostScript names (fixed in 943db65 and 1c6e35e).** Every face on both personas now answers `local()` by its real full and PostScript name (`Arial Bold`, `Arial-BoldMT`, `Segoe UI Bold`, `SegoeUI-Bold`): the Windows table carries the real names, and the unstamped faces take theirs from the source face's style.
- **VNC mode (fixed in 62bf16e).** VNC mode no longer forces `--use-angle=swiftshader`; Xvnc serves GLX, so it renders on the same ANGLE-on-llvmpipe path as plain mode, and the final gates read default and VNC mode identical on every probed field.
- **Window geometry (fixed in 918cc4d).** Real maximized Chrome 154 on Windows reads chromeWidth 0, chromeHeight 87, screenX/Y 0/0 and event offset (0, 87), the same shape as macOS; every persona now uses the system window frame and openbox leaves the Chromium window undecorated, so both personas read exactly that. The 15/94 in the real baseline is a restored window. Remaining: macOS WebGL extension counts 35/31 (real 39/36), the unmapped shader translating to GLSL, and no WebGPU adapter.
- **Canvas noise is keyed on the requested rect.** A crop, a 1x1 read or a float16 read of the same pixels is noised differently from the full read, so the reads disagree with each other.
- **Persona speech and barcode APIs are shallow.** `speechSynthesis.speak()` with a persona voice fails with `synthesis-failed`, and `BarcodeDetector.detect()` returns `[]`.
- **WebGL caps and shader dialect are tables, not a backend.** Validation and execution still follow the real ANGLE-on-llvmpipe backend; closing that needs a real persona GPU backend.
- **WebGL2 PIXEL_PACK reads are not noised.** A `readPixels` into a pixel-pack buffer skips the readPixels noise.
- **WebGPU limits and features come from the host adapter**, not the persona GPU.
- **Proxied seeds carry a STUN tell.** The srflx is fabricated, so a page's own STUN server sees no binding request yet a srflx appears.
- **Two peer connections in one page cannot connect in forced-IP mode.** Real Chrome connects them via mDNS; the `.local` names are never registered.
- **The console getter guard trusts builtins.** 0056 treats any builtin accessor as inert without proving it cannot re-enter page script during a console preview.
- **Persona DPR ignores browser zoom.** `devicePixelRatio` stays at the persona value when the page is zoomed.

Still to measure on real hardware after the build: Windows Intel/AMD iGPU caps, Windows dark-scheme Highlight, menu-bar height on real MacBook Airs, Air HDR/30-bit, and the dabi flags on both personas.

Ground rules come from [AGENTS.md](../../AGENTS.md) and [packages/browser/README.md](../../packages/browser/README.md): KISS, the public-repo rules, the golden tripwire, the patch-series contract, and one file per patch.

## Launch and image tells found at the final gates (2026-09-29)

These were measured on the 154 release binaries through the image entrypoint and `cuttle serve`, against the real Chrome 154 baselines. Every one is in how the browser is launched or how the image runs X and the harness, so every fix here is Go, image or harness only. The durable parts are in two findings: [launch and X-server tells](../knowledge/findings/container-launch-tells.md) and [the container canvas raster path](../knowledge/findings/container-canvas-raster-path.md).

1. **Keymap reset (d2cad14).**
   - Xvfb and Xvnc reset when their last client disconnects, and reload the default keymap.
   - `xkbcomp` was the only client when the persona keymap was uploaded, so the upload "succeeded" and vanished: IntlBackslash stayed `<`.
   - Fix: both X servers now run with `-noreset`.
   - Result: all 48 `getLayoutMap()` keys equal real Chrome 154 on both personas (IntlBackslash `\` on Windows, the section sign (U+00A7) on macOS).
2. **--no-sandbox infobar (f903580).**
   - The default (non-VNC) mode showed Chrome's "unsupported command-line flag: --no-sandbox" infobar. It was visible in the viewer, and pages read it as chromeHeight (outerHeight - innerHeight) 147.
   - Only the VNC entrypoint branch passed `--test-type`.
   - Fix: `--test-type` is now in the default stealth args on every launch.
   - Result: chromeHeight is 91 on Chrome's own frame; the system frame (item 4) takes it to 87, real maximized Chrome 154 on both personas.
3. **Canvas raster path (ce09c73).**
   - In the container, 2D canvas runs Ganesh GL on ANGLE over Mesa llvmpipe (maxMsaaSamples 4). It draws the CreepJS low-entropy arc with the MSAA path renderer, which gives 128/191/64, the real Mac value.
   - Real Windows draws the arc with analytic AA: 178/247/56. CPU Skia would give 192/244/53, matching neither. 151 had the same gap.
   - Fix: `--msaa_is_slow`, a GPU driver-bug workaround that Chrome itself sets on Intel, reproduces Windows exactly, with no WebGL side effects. It is passed on the Windows persona only.
4. **Window frame (1844a9c, 918cc4d).**
   - Chrome's Linux custom frame adds 4px per side, so chromeWidth read 8 and chromeHeight 91, where real maximized Chrome 154 reads 0 and 87 on both macOS and Windows.
   - Fix: the daemon seeds `browser.custom_chrome_frame=false` (the system frame) on every persona, including into existing profiles that lack it, and openbox leaves the Chromium window undecorated with no border.
   - Result: 0/87 on both personas, plain and VNC, exactly real.
5. **measureText noise (off).**
   - Patch 0055's per-seed measureText scale moves widths off Chrome's exact fixed-point grid: Arial measures 410.03154 against real 410.03125 (26242/64). A one-line check catches it: real widths are exact binary fractions, and the noised one is not.
   - Fix: cuttle no longer passes `--fingerprinting-canvas-measuretext-noise`. The persona font packs already make widths exact, so the noise buys nothing.
6. **The gate measured a different browser from the one the daemon launches (e087c2c, 14e0a3a).**
   - Fresh detect profiles lacked the daemon's stock prefs: canMakePayment basicCard, `<a ping>` link auditing, and the hidden bookmark bar.
   - detect lacked `--fingerprint-webrtc-ip`.
   - The gate started its own Xvfb, which bypassed the entrypoint keymap and showed the --no-sandbox infobar.
   - Fix: detect.py seeds the prefs `serve` writes (snapshotted in `internal/serve/testdata/fresh-profile-prefs.json`) and forces the WebRTC IP as pool.go does. The keymap moved into `ops/docker/bin/xkb-persona-keymap.sh`, which the gates share.
   - A daemon-level probe (detect.py attached to `cuttle serve`'s CDP endpoint) is now one of the final gates.

## Summary and recommended sequence

Every item was checked against our own code and the 154 tree on the build box. Three results change the plan:

- **Item 11 is not a live bug.** It is a redundant flag pair, and the fix is to delete it.
- **Item 4's cause is broader than the finding says.** Ungoogled makes `disable_non_proxied_udp` the default preference for every seed.
- **Item 1's CreepJS lies have exact, cheap triggers.** Rects noise is caught by a known-geometry check whatever the seeding, so we drop it rather than seed it.

Sequence:

1. **Day 0, no rebuild.**
   - Lane T: build tooling.
   - Lane M: probes shared by realref.py and detect.py.
   - Measure the "before" state on real Chrome (the Mac and the Windows PC) and in the image on both gate hosts.
   - Three timeboxed spikes (M3).
2. **Track A, Go and image only.** Each item is a separate PR and ships through the normal release: 6, 10 (plus the 8 GB half of 8), dropping the rects-noise flag, prefs seeding (c), and 7a if its spike succeeds.
3. **Build cycle 1** (known-shape C++). Parallel lanes, one merged series, one prep, both targets.
4. **Gate.** x64 smoke on the build box; arm64 smoke and detect macos on the Mac; detect windows on the amd64 gate host. Compare against realref.
5. **Build cycle 2** (measurement-gated or larger): WebGL/WebGPU capabilities (3), the shader dialect (2), and fixes from cycle 1.
6. **Release.** The browser tag, the pin and the binary-dependent Go changes in one PR, posture.json, the image, and the amd64 deployment gate.

Cycle 1 follows a train model: its merge cutoff is fixed, and a lane that misses it rides cycle 2.

## Lanes

Rebuild radius counts compile units (TUs) edited, excluding the two chrome links, which dominate a warm cycle at a few minutes each per target. "0" means no Chromium rebuild.

| Item | Kind | Files (154) | Depends on | Lane | Rebuild radius | Effort |
|---|---|---|---|---|---|---|
| T tooling | script | [build-linux.sh](../../packages/browser/build/build-linux.sh), Justfile | - | T | 0 | 0.5d |
| M probes | python | [realref.py](../../packages/browser/benches/realref.py), [detect.py](../../packages/browser/benches/detect.py), new `benches/probes.py`, [smoke.py](../../packages/browser/validate/smoke.py) | - | M | 0 | 1d |
| 1 canvas/measureText | C++ new 0055 + Go | static_bitmap_image.cc, image_encoder.cc, base_rendering_context_2d.cc, text_metrics.cc; [args.go](../../packages/cuttle/internal/fingerprint/args.go) (drop rects flag) | M | C-canvas, A | ~4 TUs | 1-1.5d |
| 2 shader dialect | spike, then C++ 0059 | webgl_debug_shaders.cc (option B) or ANGLE plus gn args (option A) | M3 spike | C-webgl | B: 1 TU; A: ANGLE translator, ~hundreds of TUs | spike 0.5d, then S or L |
| 3 WebGL/WebGPU caps | C++ extend 0016 + new 0058, 0060; Go pool alignment | webgl_rendering_context_base.cc, webgl2_rendering_context_base.cc, new header-only table, webgpu/gpu.cc | M, A2 | C-webgl | ~4 TUs | 2-3d |
| 4 WebRTC | C++ new 0057 + Go | peer_connection_dependency_factory.cc, third_party/webrtc/p2p/base/port.cc, stun_port.cc; [pool.go](../../packages/cuttle/internal/serve/pool.go) | M | C-webrtc | 3 TUs (no header edits) | 1.5-2d |
| 5 CDP | C++ new 0056 | v8/src/inspector/value-mirror.cc | M | C-cdp | 1 TU plus relink | 0.5d |
| 6 keyboard map | image | [docker-entrypoint.sh](../../ops/docker/bin/docker-entrypoint.sh) | M (real maps) | A | 0 | 2h |
| 7a sample rate | image + Go, else C++ 0061 | spike: asound.conf plus `--audio-buffer-size`; fallback: content/renderer/media/renderer_webaudiodevice_impl.cc | M3 spike | A or C-audio | 0 or 1 TU | 0.5d |
| 7b audio noise | C++ rewrite of 0026 | drop the audio_buffer.{h,cc} hunks; add offline_audio_destination_handler.cc | M | C-audio | 3 TUs | 0.5-1d |
| 8 heap limit | Go (A2) + C++ new 0064 | core/timing/memory_info.cc | A2 | C-display | 1 TU | 0.5d |
| 9 persona CSS | C++ new 0062 + extend 0011, 0054 | core/layout/layout_theme.cc, core/frame/screen.cc, core/css/media_values.cc | M | C-display | 3 TUs | 1d |
| 10 integrated GPUs | Go | args.go `windowsMachines`, golden | M (tables) | A | 0 | 2h |
| 11 referrers | C++ edit 0040, 0019 + Go | net/base/features.cc, services/network/public/cpp/features.cc, third_party/blink/common/features.cc; args.go | - | C-net | 3 TUs | 2h |
| shared header | C++ 000-shared + 0050 | cuttle_fingerprint_switches.{h,cc}, render_process_host_impl.cc | - | C-net (single owner) | ~25 includers | 1h |
| (a) availTop | C++ extend 0011 | screen.cc | M | C-display | shares 9's TU | 2h |
| (b) system-ui | C++ new 0063 | platform/fonts/font_cache.cc (`SystemFontPlatformData`) | M | C-display | 1 TU | 0.5d |
| (c) default prefs | Go | pool.go `seedProfileDefaults` | M | A | 0 | 0.5d |
| (d) delete 0002/0045 | series | headless/lib/... (3 files) | T1 | C-net | 3 TUs | 1h |
| (e) ReportingObserver | measure, then maybe gn | args.gn `enable_reporting` | M | M | 0, or heavy if flipped | 1h measure |

Parallelism:

- A, T and M run at once from day 0. The C lanes can start drafting hunks from pristine 154 files (copied off the box read-only with `ssh ... cat`) as soon as M has fixed the probe definitions.
- The C lanes own disjoint files. There are two single-owner rules:
  - C-net alone edits 000-shared and 0050, so the widely-included header changes once.
  - C-webgl alone edits 0016.
- The one shared patch file is 0054: C-display edits its media_values.cc section. C-canvas avoids document.cc (0054's other file) by rewriting noise at the call sites instead.
- Merge: each lane delivers its `.patch` files, their round-trip proof (T3) and its probe additions. The integration branch runs the T2 lint, then one prep on the box.

## Tooling (Lane T) - where it pays off

- **T1. Apply with `git apply` in build-linux.sh stage 4** (`git apply --check`, then `git apply`, in build/src).
  - It refuses zero-context hunks unless `--unidiff-zero` is passed, which closes the `-F0` hole.
  - It is atomic per patch, so a failed apply leaves no partial state and no longer needs the full tree reset the README describes.
  - Record each applied patch as `.browser-applied/<name>.<hash>.patch`. When a patch's hash changes, or the patch leaves the series, `git apply -R` the recorded old copy first. This reverts exactly the files it touched, including files a rewritten patch no longer touches (0026 drops audio_buffer.*), instead of hitting today's FATAL and a full reset.
  - Why it matters: this is what keeps out/ warm across cycles and makes deleting 0002/0045 cheap. Without it, deleting a patch forces a whole-tree reset, which bumps the mtime of every patched file and rebuilds all ~80k targets (sccache absorbs it, but it costs hours of cache lookups).
  - Verify on the 154 tree that `git apply` from build/src handles paths inside v8/ and third_party/webrtc, which are nested git repos (0056 and 0057 need this). Fallback: `git -C v8 apply -p2`.
- **T2. `just patch-lint` in CI:** `grep -cE '^@@ -[0-9]+,0 \+[0-9]+' packages/browser/patches/0*.patch` must be all zero. Cheap and needs no build box.
- **T3. Regen round-trip.** A script (`packages/browser/build/regen-patch.sh`) that does `diff -u orig new > p`, `git apply p` onto a copy of orig, then `cmp copy new`, and also diffs the patch's added-identifier sets against its previous version. This catches the 151-era dropped `0016` hunk. Every C lane uses it before handing in a patch.
- **T4. Copy 000-shared in stage 5 only when the content changed.** Today `cp -f` restamps it on every prep, so all ~25 includers recompile. Do not use `cp -p`: an old source mtime could hide a real edit from ninja.

## Measurement first (Lane M)

- **M1.** Move every probe's JS into `benches/probes.py`, imported by both realref.py and detect.py. It stays dependency-free, so the Windows box needs nothing new. It includes a tiny two-origin local HTTP server (ports A and B on 127.0.0.1), used by the referrer and UA-CH header probes. Both scripts emit `metrics["probes"]`; `detect.py --merge` needs no change. Record the raw integers (e.g. `jsHeapSizeLimit`), not only rounded GB.
- **M2. "Before" runs.**
  - realref.py on the real Mac and the real Windows PC (Chrome 154).
  - detect.py on 154-1: macos on the Mac, windows on the amd64 gate host.
  - Commit the result as the posture.json baseline for 154.
- **M3. Spikes, timeboxed at half a day each.**
  - (i) Extract the `hasInconsistentWebGLShaderLang` rule from dabi's detector script, and capture real Chrome's `getTranslatedShaderSource` output for that rule's shaders on Mac and Windows.
  - (ii) ALSA null sink in the image (`pcm.!default {type null}`) plus `--audio-buffer-size`: does `AudioContext` read 48000 / 0.01?
  - (iii) On the 154 container (headed, as root): does `navigator.gpu.requestAdapter()` return null, or a CPU/fallback adapter?
  - (iv) ReportingObserver under `enable_reporting=false`.
  - (v) `canMakePayment` and prefs on a fresh profile against stock Chrome.

## Per-item sections

### 1. Canvas, measureText and client-rects noise (issue #80)

**Verified.**

- On 154, `base::RandDouble` and `RandIntInclusive` are still at static_bitmap_image.cc:137/153/161, and the per-Document factor at document.cc:1067-1068. Both come from ungoogled's bromite patches; nothing in our series touches them.
- The async encode path (canvas_async_blob_creator.cc:521/529 `ImageEncoder::Create`) bypasses the noised `ImageEncoder::Encode`, so `toBlob` and OffscreenCanvas `convertToBlob` read back un-noised. That is a cross-API mismatch.
- The CreepJS lies have exact triggers:
  - getImageData: after `clearRect`, `getImageData(0,0,8,8)` must be all zeros (canvas/index.ts:440). Bromite perturbs RGB even on alpha-0 pixels.
  - measureText: `measureText('')` must return integer font-box values (index.ts:497-527). Bromite scales fontBoundingBox and em/baseline values.
  - getClientRects: a rotated 100px square must hash to a known value at DPR 1 (domrect/index.ts:300-315). Any geometry noise fails this, which is why posture.json shows the lie on the Windows persona (DPR 1) and not on macOS (DPR 2).

**Implementation.**

- **Go, Track A:** remove `--fingerprinting-client-rects-noise` from ForkParityArgs. Update smoke.py (TestSmokeMatchesProductionFlags pins it) and the golden. Update the README's "Canvas noise is detectable" section: rects stay per seed through each seed's own screen and window size, like identical real laptops. This also closes #80's getBBox half by deletion, because rects and getBBox then agree un-noised.
- **New patch 0055-canvas-noise-seed-stable** (lane C-canvas):
  - static_bitmap_image.cc: replace the RNG with splitmix64 seeded by `cuttle::seed::Hash("canvas.image-data") ^ base::FastHash(pixels)`. Skip pixels with alpha 0 and pixels equal to all four neighbours; keep bromite's size and count shape.
  - image_encoder.cc: apply the same noise in `ImageEncoder::Create` (streaming, which covers toBlob and convertToBlob) on the copy it encodes, as `Encode` already does.
  - base_rendering_context_2d.cc: measureText factor from `cuttle::seed::Hash("measuretext")` instead of `Document::GetNoiseFactorX()`, which leaves document.cc to 0054.
  - text_metrics.cc: `Shuffle` scales only `width` and `actual_bounding_box_*`.
  - Draft in #80. Include `third_party/blink/common/cuttle_seed.h` the way 0026 does; confirm `gn check` from platform/, or compute the key in the modules/ callers.

**Validation.**

- smoke.py, both personas:
  - (a) Same seed: 3x toDataURL, 3x getImageData hash and 2x measureText are identical, and identical again after a relaunch.
  - (b) Seeds 1 and 42069 differ.
  - (c) `clearRect` then 8x8 read gives max 0.
  - (d) `measureText('')` font-box values are integers.
  - (e) toDataURL equals the dataURL of convertToBlob and toBlob. If the encoders differ byte-wise, compare decoded pixels instead.
- realref and detect: probe `canvas` gives the same stable/clean booleans on real Chrome.
- posture: `creepjs.lies` loses `CanvasRenderingContext2D.getImageData`, `CanvasRenderingContext2D.measureText` and `Element.getClientRects` on both personas.

**Risks.** Hashing the pixels costs O(pixels) per readback, the same order as the readback itself. The streaming encoder must not mutate the canvas backing store; assert on a copy.

### 2. WebGL shader-language leak

**Verified.**

- webgl_debug_shaders.cc:46-56 returns the ANGLE backend's own translation (Vulkan/SwiftShader on Linux).
- The README records byte-identical identity strings between a real Mac and ours, so the detector reads something underneath.
- That dabi keys on `getTranslatedShaderSource` comes from our probe, not from dabi's source: verify it first (M3 i).

**Implementation.** Decide after the spike.

- Option B (1 TU, patch 0059 on webgl_debug_shaders.cc): if the rule is a dialect-marker check, emit the persona dialect (MSL on macOS, HLSL on Windows) for the shaders the detector compiles, based on real captures.
- Option A: run ANGLE's MSL/HLSL translator for the persona via `angle_enable_msl` / `angle_enable_hlsl` on Linux. That is a gn args change, an ANGLE translator rebuild and a GPU-process patch; heavy.
- Do not drop the extension; real Chrome exposes it.

**Validation.** Probe `shader`: the dialect class and the first 200 characters of the translated source for fixed vertex and fragment shaders; realref on both real machines; detect on both personas. posture: macos `are_you_a_bot.flagged` loses `hasInconsistentWebGLShaderLang`.

**Risks.** Option B deceives only the known shapes. If the spike shows the rule needs a real translator, defer the item and keep it recorded as a known cost.

### 3. WebGL/WebGPU capabilities read as SwiftShader

**Verified.**

- 0016 and 0049 spoof strings only.
- adryfish 011-gpu-info (BSD-3) carries captured tables for 9A49 and 3EA0, which are exactly in our pool, and for 46A6, A7A0, 9B41, AMD 1638 and Apple M2/M4.
- Our pool uses 46A8, A7A1, 9BC8 and AMD 1636, which adryfish lacks.

**Implementation.**

- **A2 (Go, together with item 10):** align `windowsMachines` to the tabled device IDs, re-verifying core and memory pairings against vendor specs. Mac: one table for every Apple M entry, captured by realref on a real Mac and cross-checked against adryfish M2/M4. Metal limits are the same across M1-M4 at this level; confirm by diffing M2 against M4.
- **C-webgl:**
  - Extend 0016 in webgl_rendering_context_base.cc: `getParameter` numeric caps, `getShaderPrecisionFormat`, and `getSupportedExtensions` / `getExtension` hide-only (never claim an extension the backend lacks, except constant-only ones).
  - New 0058 for webgl2_rendering_context_base.cc (WebGL2 caps).
  - The table lives in a new header-only file added by 0058, keyed on the renderer string. Keep the BSD-3 notice.
- **WebGPU:** if M3 iii shows a CPU/fallback adapter, new 0060 in webgpu/gpu.cc resolves `requestAdapter` to null when a persona is active and the adapter is a fallback. The README already treats an absent adapter as a pass. Do not spoof limits.

**Validation.** Probe `webgl_caps` (a parameter list, precision formats, the sorted extensions for gl1 and gl2) and `webgpu` (null, or fallback/info/limits). realref on the real Mac and Windows PC. detect macos on the Mac, windows on the amd64 gate host. The expectation is equality with the real table for that GPU. posture: new `probes.webgl_caps` diff counts go to 0.

**Risks.**

- A claimed MAX_TEXTURE_SIZE of 16384 over SwiftShader's 8192 fails a real allocation. That is rare, and accepted.
- Only the one Windows GPU on the real PC can be checked against a live reference; the others rely on adryfish's captures.

### 4. WebRTC: `--fingerprint-webrtc-ip` is a no-op

**Verified.**

- `kFingerprintWebrtcIp` is only forwarded, by 0050; no patch reads it. The Go side still resolves an exit IP for it (pool.go:490-501, proxy.go:84).
- Zero candidates everywhere has a simpler cause: ungoogled's `default-webrtc-ip-handling-policy.patch` registers `disable_non_proxied_udp` as the default preference. That applies to every seed, not only proxied ones.
- `enable_mdns=false` is set in args.gn, so a `.local` host candidate can never come from the real responder.

**Implementation.** New 0057-webrtc-fabricated-candidates. It is ported from clearcote 100-webrtc-leak (BSD-3, notice kept), with two changes: fabricate a `.local` host candidate, and never send.

- peer_connection_dependency_factory.cc: read `kFingerprintWebrtcIp` and set the forced IP. When it is set, force UDP port creation.
- port.cc: in forced mode, host candidates carry a random-UUID `.local` hostname instead of being dropped.
- stun_port.cc: fabricate the srflx at the forced IP with raddr `0.0.0.0:0`, send no binding request, and block all UDP sends from UDPPort in forced mode. With no connectivity checks from the host interface there is no packet-level leak.
- Declare the accessors in port.cc and as `extern` in the two consumers, so port.h (a widely-included webrtc header) is not edited.

Go:

- Pass `--fingerprint-webrtc-ip` for direct-egress seeds too (keep the IP `directEgressGeo` already resolves).
- When the IP is set, omit the two policy flags.
- When no IP resolves, keep the policy (fail closed: zero candidates).
- Regenerate the golden. This Go change ships with the binary pin.

**Validation.**

- Probe `webrtc`: an `RTCPeerConnection` with `stun:stun.l.google.com:19302`; record candidate types, whether the host candidate ends in `.local`, and the srflx IP.
- Real reference: realref on both machines (one `.local` host candidate plus a public srflx).
- Ours: smoke passes `--fingerprint-webrtc-ip=203.0.113.7` and expects host `.local` plus srflx 203.0.113.7.
- Leak check, ours only: a python UDP listener used as the ICE server must receive 0 packets. Real Chrome sends a binding request, so it is the negative control.
- posture: new `probes.webrtc`.

**Risks.** Real-time media never connects in forced mode, the same as today's zero candidates. A caller-pinned `--webrtc-ip-handling-policy` on the connect URL does not restore real ICE, so it is not documented as an override; media connects only through a TURN server over TCP or TLS.

### 5. CDP visible to the page (issue #50)

**Verified.**

- detect.py and smoke.py never send `Runtime.enable` (`Runtime.evaluate` only), so they cannot see this.
- On 154, `getErrorProperty` (value-mirror.cc:271-300) avoids own accessors, but falls back to `object->Get` for inherited ones. So a getter on `Error.prototype.name` fires during console preview: 4 reads through playwright-cli against 1 with no debugger attached (#50).

**Implementation.**

- New 0056-cdp-console-preview-getter-guard: in `getErrorProperty`, walk the prototype chain with `GetOwnPropertyDescriptor` and return only data values, never invoking accessors.
- Do not port adryfish 001 or clearcote 110: they disable `addBindings` and `messageAdded`, which breaks driver console capture and bindings.
- The debug-port probe (a no-cors fetch to 127.0.0.1:port) is measure-only, since Local Network Access already blocks public origins on 154. Revisit only if a public-origin probe succeeds.

**Validation.**

- smoke.py and detect.py gain a "driver-shaped" section that sends `Runtime.enable` on the page socket, then runs the #50 getter-count probe in the page and in a worker. Expected: 1, which is realref's value (realref never enables Runtime).
- Manual per-driver check at release step 9: the same probe through the bundled playwright-cli.
- posture: `probes.cdp.nameGetterReads`, and dabi `isAutomatedWithCDP` with Runtime enabled.

**Risks.** It edits the v8 nested repo (T1 check). Timing-based detection (stack capture while enabled) remains; record it as a known gap.

### 6. Xvfb keyboard layout map

**Verified.** The entrypoint starts Xvfb or Xvnc with no keymap setup (docker-entrypoint.sh). Chrome on X11 reads the server keymap through XKB (ui/events/ozone/layout/xkb). The "impossible" claim comes from the probe; M will show the exact difference.

**Implementation, image only.** After the X-ready poll, run `setxkbmap -display :99` with the model and layout that reproduce the real map (expected `-model pc105 -layout us`). Select a mac variant on arm64 only if the real Mac map differs. Applies to both the Xvfb and the Xvnc branch. setxkbmap ships with xvfb's x11-xkb-utils dependency; verify, or add it to apt.

**Outcome.** `setxkbmap -model pc105 -layout us` was a no-op. What shipped is a per-persona IntlBackslash remap loaded with xkbcomp (`ops/docker/bin/xkb-persona-keymap.sh`), and it only sticks with the X server started with `-noreset` (tell 1 under "Launch and image tells found at the final gates").

**Validation.** Probe `keyboard`: the sorted `navigator.keyboard.getLayoutMap()` entries (secure context). realref on both machines, detect on both personas; expect equality. posture: `probes.keyboard.diff` = 0.

### 7. Audio: 44.1 kHz and AudioBuffer noise

**Verified.**

- A device-less renderer gets `UnavailableDeviceParams` (44100 Hz, 441 frames; media/base/audio_parameters.cc:367-375). baseLatency is frames divided by rate (audio_context.cc:674-679).
- `kFingerprintAudioSampleRate` is declared and forwarded but read by nothing.
- 0026 adds a per-seed offset to every sample of every AudioBuffer on first read, including user-constructed buffers and zeros. That trips CreepJS `hasFakeAudio` (audio/index.ts:60-74) and the write/readback trap (260-300), hence the `AudioBuffer` lie on both personas.

**Implementation.**

- 7a: Track A if the M3 ii spike gives 48000 Hz and the measured real baseLatency (Windows 0.01; Mac per realref). Otherwise new 0061 in renderer_webaudiodevice_impl.cc sets 48000 Hz and the measured buffer when a persona is active; idea from apostate 0130, written clean-room.
- Delete `kFingerprintAudioSampleRate` (C-net).
- 7b: rewrite 0026 in place. Remove the audio_buffer.{h,cc} hunks; add a multiplicative-only per-seed scale in `OfflineAudioDestinationHandler::DoOfflineRendering` (offline_audio_destination_handler.cc). Zeros stay zero, the two read paths agree, and user buffers are untouched. The pattern follows chromiumfish audio (MIT) minus its additive term, which breaks `hasFakeAudio`.

**Validation.**

- Probe `audio`: `new AudioContext()` sampleRate and baseLatency, `hasFakeAudio`, the getChannelData/copyFromChannel match, the user-buffer trap, and the offline sum.
- realref on both machines.
- The existing seed differential stays in smoke. Add "same seed, two launches, identical sum" and "zero oscillator all zero".
- posture: `creepjs.lies` loses `AudioBuffer`; `probes.audio.sampleRate` = 48000.

### 8. jsHeapSizeLimit follows the host

**Verified.**

- memory_info.cc:54 reports V8's host-derived `heap_size_limit`, quantized at :102.
- posture.json shows ours equal to real (4.09) on both bench hosts, which have 16 GB or more.
- The tell appears on smaller hosts (a Docker Desktop VM on a Mac) and for the two 8 GB entries in `windowsMachines` (args.go:468, 472) when the host is large.

**Implementation.**

- A2 (Go): lift those two entries to 16 GB, which is plausible for both SKUs.
- New 0064 in memory_info.cc: when a persona is active, set the pre-quantization limit to the value our binary reports on a host with 16 GB or more. Capture it raw on the amd64 gate host with `--enable-precise-memory-info`.
- No gin/V8 change, so no GC behaviour change.

**Validation.** Probe `heap`: the raw `jsHeapSizeLimit` plus `deviceMemory`. realref raw on both machines. detect macos on the Mac inside a Docker VM with less than 16 GB: it drops before the fix and equals real after. posture: `basics.heapLimitGB` plus `probes.heap.raw`.

### 9. Persona CSS and display

**Verified.**

- `hasKnownBgColor` fires on our Windows persona and not on real Windows, but does fire on real Mac, so the Mac persona is right to fire it.
- `kActivetext` resolves in layout_theme.cc:643/781.
- smoke asserts colorDepth 24 on both personas.

**Implementation.**

- New 0062 in layout_theme.cc, Windows persona only: the system colours whose values differ from real Windows, per probe `css_colors`. Confirm which of the two switch sites is live on 154.
- Extend 0054 (media_values.cc): `color-gamut: p3` and `dynamic-range: high` for macOS, if realref shows them.
- Extend 0011 (screen.cc): colorDepth/pixelDepth 30 for macOS if real reports 30, plus item (a) availTop.
- Update the smoke assertions to match.

**Validation.** Probe `css`: computed colours for the CSS system-colour keywords, gamut/HDR media queries, colorDepth, and availTop/availLeft. realref on both machines. posture: windows `creepjs.likeHeadless` loses `hasKnownBgColor`; `probes.css` diff = 0.

### 10. Prefer integrated GPUs

**Verified.** The pool is 6 integrated and 2 discrete (args.go:459-476).

**Implementation (Go, A2):** remove the RTX 3060 and RX 7600 entries, plus the ID alignment from item 3. Regenerate the golden; detect.py uses the first entry.

**Validation.** Golden diff review, plus detect windows on the amd64 gate host. Expect no change in CreepJS or dabi; botstop stays HUMAN.

### 11. Referrer features in 0040

**Verified: neutralised in production.**

- The ungoogled sanitizer (`add-flags-for-referrer-customization.patch`) consults FeatureList in the browser, the network service, the renderer and the workers. `--disable-features=...MinimalReferrers,NoCrossOriginReferrers` (args.go:325) overrides the defaults 0040 flips, and feature overrides propagate to child processes.
- Residual 1: the binary is wrong without the flag.
- Residual 2: ForkParityArgs is appended after the caller's args (pool.go:503) and BuildArgs keeps the last value per key, so a caller can never disable a feature of its own.

**Implementation.**

- Remove the two referrer hunks from 0040 (keep `SetIpv6ProbeFalse`) and the `kRemoveClientHints` flip from 0019.
- Then delete `--disable-features` from ForkParityArgs and smoke.py, and regenerate the golden. This Go part ships with the binary pin, never before it.

**Validation.** Probe `referrer` (two-origin local server): the cross-origin Referer is origin-only with a trailing slash; same-origin is the full URL. Probe `uach_headers`: Sec-CH-UA headers on the wire. realref on both machines. Assert both in smoke. posture: `probes.referrer`.

### Folded-in rebuild-deferred items

- **(a) macOS availTop** ([#45](https://github.com/glim-sh/cuttle/issues/45)). Rebuild; extend 0011. availTop is the menu-bar height and availHeight is unchanged. The height itself comes from realref; notch models differ. Smoke assertion: availTop > 0 on macOS and 0 on Windows. The DPR half of #45 is already fixed by 0054.
- **(b) system-ui resolves to Verdana on macOS.** Rebuild; new 0063 in font_cache.cc (`FontCache::SystemFontPlatformData`), keyed on persona: macOS the hidden `sysui-q7k2` face (SF Pro Text metrics), Windows "Segoe UI" (see Decisions after drafting; the original Helvetica proposal was dropped). Probe: the measureText width of `system-ui` against real.
- **(c) ungoogled default prefs** (#53 item 4). No rebuild, Go: `seedProfileDefaults` writes `payments.can_make_payment_enabled`, the password-manager and auto-signin prefs, card autofill, and bookmark-bar visibility, following the `cookie_controls_mode` precedent. Re-measure first (M3 v). The bookmark bar also moves the gap between outerHeight and innerHeight.
- **(d) Delete 0002 and 0045** (plan K5). Rebuild, cheap with T1. Read the source first to confirm they are headless-only; the README already says so.
- **(e) ReportingObserver** (K1). Measure only (M3 iv). If it is missing, drop `enable_reporting=false` from args.gn in cycle 1. That is heavy: net reporting TUs are cold in sccache.

### Larger (not in this release)

- GCM/push per-seed check-in: a product decision; rebuild.
- FLEDGE `joinAdInterestGroup` hanging (#53 items 2-3): rebuild.
- ICU default locale in the engine, extending 0005 (idea: clearcote 092, BSD-3), then remove `Emulation.setLocaleOverride` from [wsproxy.go](../../packages/cuttle/internal/serve/wsproxy.go). Measure `--lang` headed first; rebuild.
- `canPlayType('hvc1')` on the arm64 macOS persona: low value; rebuild.
- ThinLTO/PGO revisit: the x64 parity reason was retired at 151. A full rebuild and a longer link.

### Measure-only

Math.tanh UCRT (K3); JA3/JA4 TLS fingerprints.

### Obsolete (closeable)

- [#24](https://github.com/glim-sh/cuttle/issues/24): fixed via RemoveClientHints. After item 11 the fix moves into the binary.
- The DPR half of #45: fixed by 0054.
- Widevine gn: `enable_widevine=true` is already in args.gn. The README "Widevine / EME" section (around lines 654-656) is stale; update it.

## Validation matrix

Hosts:

- Mac: the arm64 Mac; the macOS persona in the arm64 image, plus real Chrome for realref.
- amd64 host: the amd64 gate host running the Windows persona.
- Win PC: real Chrome on Windows.
- box: the Stage 7b x64 smoke.

| Probe | Item | Where | Ours: persona / host | Real reference | posture.json field |
|---|---|---|---|---|---|
| canvas | 1 | smoke, detect, realref | both / box (x64), Mac, amd64 host | Mac, Win PC | creepjs.lies, probes.canvas |
| shader | 2 | detect, realref | both / Mac, amd64 host | Mac, Win PC | are_you_a_bot.flagged, probes.shader |
| webgl_caps, webgpu | 3 | detect, realref | both / Mac, amd64 host | Mac, Win PC (plus adryfish tables) | probes.webgl_caps, probes.webgpu |
| webrtc (+ UDP listener) | 4 | smoke, detect, realref | both / box, Mac, amd64 host | Mac, Win PC | probes.webrtc |
| cdp getter count | 5 | smoke, detect (Runtime.enable) | both / box, Mac, amd64 host | Mac, Win PC (value 1) | probes.cdp, are_you_a_bot.flagged |
| keyboard | 6 | detect, realref | both / Mac, amd64 host (image) | Mac, Win PC | probes.keyboard |
| audio | 7 | smoke, detect, realref | both / box, Mac, amd64 host | Mac, Win PC | creepjs.lies, probes.audio |
| heap | 8 | detect, realref | both / Mac (Docker VM under 16 GB), amd64 host | Mac, Win PC | basics.heapLimitGB, probes.heap |
| css (+ availTop, system-ui) | 9, a, b | smoke, detect, realref | both / box, Mac, amd64 host | Mac, Win PC | creepjs.likeHeadless, probes.css |
| pool | 10 | golden, detect | windows / amd64 host | Win PC | botstop, creepjs |
| referrer, uach_headers | 11 | smoke, detect, realref | both / box, Mac, amd64 host | Mac, Win PC | probes.referrer |
| prefs, reportingobserver | c, e | detect, realref | both / Mac, amd64 host | Mac, Win PC | probes.prefs |
| ua (page, worker, header, brands), cpuPerformance | ua | smoke, detect, realref | both / box, Mac, amd64 host | Mac, Win PC | probes.worker, probes.ua |
| fonts (system-ui, BlinkMacSystemFont, -apple-system, hidden name, Segoe UI) | b, segoe | smoke, detect, realref | both / box, Mac, amd64 host | Mac, Win PC | probes.fonts |
| keyboard (IntlBackslash) | 6 | detect, realref | both / Mac, amd64 host | Mac, Win PC | probes.keyboard |
| cdp driver-shaped getter reads | 5 | smoke, detect, cdp-getter-probe.sh | both / box, Mac, amd64 host | Mac, Win PC | probes.cdp |

## Release and rollout

This supersedes the earlier Track A / cycle 1 / cycle 2 sequence. All work ships in one release.

Parallel prep while the first builds run:
- The integration lane merges every lane branch into `feat/stealth-154-integration`, round-trips the whole series against the box tree, and syntax-checks every changed .cc with the box clang and real x64 flags. Each check runs in a throwaway `docker run --rm -i` from the deps image with the tree mounted read-only, at normal priority: at `nice -n 19` on a box at load 98 one check took 8-10 min instead of under one, and a `docker exec` into a build container dies when that build exits.
- The image font stages (Inter/SF face on arm64, Selawik/Segoe UI on amd64) are built and checked ahead of the binary.
- The gate hosts are synced and dry-run against 151: the Mac for arm64/macOS, the amd64 gate host for amd64/Windows.
- The release pin commit is scripted, so pinning is one step.
- Real Chrome 154 baselines (realref) are captured on the Mac and the Windows PC as soon as the integration probes exist, not after the build. The Windows run must be on an unlocked console (see the peer-survey finding). Final captures use the integration probes at 36af8d3 on real Chrome 154.0.8037.58, both clean (isBot false, botstop HUMAN 0, CreepJS 0/0 with no lies). The Mac baseline host has SF Pro hand-installed in /Library/Fonts, so its `font_names` "SF Pro" = present is host-specific; stock macOS reads it absent, as our persona does.
- The PR body and release notes are drafted and linted against the release-note rules.

Box runbook:
1. Both first 154 builds run to completion. The x64 smoke runs in Stage 7b; no detector gates run, because of the 151 UA literal. The arm64 watcher (out-of-repo tooling) reruns its build stage once if it exits red. arm64 is not stopped early: the remaining work is the same either way and arm64 sets the release date, so stopping only adds contention.
1b. Early x64 in a copy of the tree, so compile errors and the x64 gates do not wait for arm64. After the x64 first build, copy the tree with `rsync -a --exclude=/out/arm64` (timestamps preserved), and prove it with `ninja -C out/x64 -n chrome` in a container that mounts the copy at the same in-container path: it must print "no work to do", like the main tree. Same path means sccache hits across trees. A launcher (out-of-repo tooling) runs the staged integration package, with the uncommitted 154 pins and golden, at `--cpu-shares=2` under its own container names and dist directory. The main tree stays untouched. Its x64 tarball feeds the Windows gates on the amd64 gate host, a local amd64 test image (the image gates) and `cdp-getter-probe.sh` early, and its objects warm sccache for the main-tree x64 rebuild. It is never shipped: both release tarballs come from the main tree, built from one commit. On 2026-09-29 the rebuild with all 36 patches was 181 steps, with 0 compile errors. Its Stage 7b smoke passed after one harness fix (`want[g]` to `want[gl]` in smoke.py). On the amd64 gate host it gave smoke 77 pass and 3 fail, parity 0 unexplained diffs and detect 13/13. The 3 failures were the Segoe UI font checks, which cannot pass against the published image's old fonts, while `system-ui` already measured identical to "Segoe UI", so 0063 worked. In a local amd64 test image with the new font pack, every gate passed: Go image smoke 8/8, `cdp-getter-probe.sh`, smoke 80/80, parity 0, detect 13/13, and system-ui 331.3478 / 407.8126 and the kerned string 159.5000 against real 331.3477 / 407.8125 / 159.5.
2. The old series (9220dcc) is saved on the box as the one-time seed. Stage the release package in a separate directory while arm64 still builds, never over the checkout the running container bind-mounts: the PR head plus the uncommitted 154 pins and the golden. A detached chain script (out-of-repo tooling) waits for the arm64 first build to exit. If it is green, the script moves the old-series tarballs aside, runs step 3 once from the staged package, and starts step 4 for both targets from that one commit. If it is red, it stops for a human.
3. Prep once, from the staged package's `packages/browser` and with the one-time seed (since removed) pointed at the old series:
   `BROWSER_ALLOW_UNMOUNTED_WORK=1 BROWSER_STAGE=prep ./build/run-build.sh foreground`
   This reverses only changed and removed patches, then applies new and changed ones.
4. Build both targets concurrently:
   `BROWSER_ALLOW_UNMOUNTED_WORK=1 BROWSER_STAGE=build TARGET_CPU=x64 ./build/run-build.sh background`, and the same with `TARGET_CPU=arm64`.
   On a compile error: fix the owning patch, rsync, prep, and rebuild (only the failed and dependent edges rebuild). Check `sccache --show-stats`.
5. Gates:
   - x64 smoke in Stage 7b;
   - each persona in a local test image that has the new binary and the new font pack (the published image's fonts cannot pass the font checks): a scratch copy of the Dockerfile that swaps only the browser `ADD` for `COPY --from=browserbin` (named build context), built on the Mac (`docker buildx build --platform linux/amd64|linux/arm64`), then a gate script (out-of-repo tooling) inside it (smoke, parity, detect), `go -C packages/cuttle run ./test/smoke` and `cdp-getter-probe.sh`. amd64 runs on the amd64 gate host, arm64 on the Mac;
   - arm64 smoke plus detect macos on the Mac;
   - detect windows on the amd64 gate host;
   - parity.py;
   - realref on the real Windows PC runs in the console session via a scheduled task, never over ssh;
   - the daemon-level probe, per persona, in that persona's test image: start the image through its own entrypoint (default mode, not VNC), let `cuttle serve` launch the browser, and attach detect.py to serve's CDP endpoint. Record the launch argv and an X screenshot next to the JSON. This is the only gate that sees the argv, the seeded prefs, the keymap and the window frame users get; the harness gates start their own browser and missed all six tells above. CreepJS, are_you_a_bot and botstop are read here, and every field that still differs from realref must be explained.
   Every posture move must head toward the realref values. Record posture.json.

   Results on 2026-09-29, on the first release candidate (x64 tarball sha256 abf1ddb0..., arm64 efe0d4e5...), both personas, each in its own test image: smoke 80 and 76 pass with 0 fail, parity 0 unexplained, detect 13/13, Go smoke 8/8, `cdp-getter-probe.sh` PASS. At daemon level, CreepJS headless/stealth/likeHeadless reads 0/0/19 on Windows and 0/0/25 on macOS, both equal to real Chrome 154; are_you_a_bot `isBot` is false, and botstop is HUMAN 0. That run came after e087c2c/14e0a3a and before the launch fixes ce09c73 to 1844a9c. Each of those fixes was verified by its own daemon-level measurement of the field it moves (keymap, chromeHeight/chromeWidth, lowEntropyImageData), and the full gate set is rerun on the image built from the pin commit (step 7).

   Two more fix rounds followed (the round-3 findings, persona fonts 0067, then `--fingerprint-webrtc-no-local-dns` in 0057). The shipped binaries are built from 69789c0 (x64 sha256 510d82eb..., arm64 d6c87e52...). On them, both personas: smoke 77/77 macOS and 81/81 Windows, parity only the intended `navigator.connection` rounding, detect 0 failed (CreepJS and likeHeadless as above, are_you_a_bot 0/5 with `Runtime.enable`, botstop HUMAN 0), the daemon probe 0 failed in pool mode, default and VNC, the session-mode checks (driver-attached are_you_a_bot, canvas readback, geometry, keymap) as on the previous candidate, Go smoke 8/8, `cdp-getter-probe.sh` PASS, and posture.json unchanged from the previous candidate. A proxied seed without a forced WebRTC IP now also fails WebRTC's own DNS lookups: 0 local lookups of a page's TURN hostname, against 8 before. Review findings that need a rebuild are tracked in [#126](https://github.com/glim-sh/cuttle/issues/126); the image built from the release head after the Go and font-name review fixes passed the same gates on both personas.
6. After sign-off, publish `browser-v154.0.8037.57-1`: both tarballs plus .sha256, with the verification block. Then one pin commit carries:
   - `BROWSER_RELEASE_TAG` and both shas in [versions.env](../../packages/browser/versions.env);
   - the Dockerfile ARG/ADD literals;
   - `chromiumVersion` in args.go;
   - the golden;
   - posture.json;
   - the binary-dependent Go changes (lane A's `--disable-features` removal, the WebRTC arg flow);
   - the README updates: patch count, canvas, WebGL, Widevine and fonts.
7. Build the image, run `go -C packages/cuttle run ./test/smoke`, run the real amd64 deployment gate, and run `validate/cdp-getter-probe.sh`. PR [#124](https://github.com/glim-sh/cuttle/pull/124) (draft) carries it; its body addresses #24 without an auto-close keyword. It is marked ready and merged only after sign-off.

   Results on 2026-09-30: #124 merged and shipped as cuttle 0.16.0. The real amd64 deployment gate ran on the published amd64 manifest `ghcr.io/glim-sh/cuttle:0.16.0@sha256:5661a081...`: 10/10 checks passed, and the 2 known bot-walled checks timed out as before. That is identical to the 0.13.1 baseline. The daemon had 0 restarts and no panics.
8. Box teardown, after the artifacts are local and published:
   - delete the 151 build tree, the 151 ungoogled-chromium checkout and the shadow copy on the box (about 74G less snapshot);
   - `hcloud server poweroff cuttle-builder`
   - `hcloud server create-image cuttle-builder --type snapshot --description "... 154.0.8037.57 ..." --label purpose=cuttle-browser-build --label chromium=154.0.8037.57`
   - wait for `available`, then enable delete protection;
   - `hcloud server delete cuttle-builder`;
   - unprotect and delete the old 151 snapshot once the 154 one exists.
