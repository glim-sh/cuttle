# Stealth improvements after the 154 rebase

Date: 2026-09-29. Scope: the 11 items in [the peer survey](../knowledge/findings/stealth-chromium-peer-survey-2026-09.md), plus the rebuild-deferred items folded in from issues [#45](https://github.com/glim-sh/cuttle/issues/45), [#50](https://github.com/glim-sh/cuttle/issues/50), [#53](https://github.com/glim-sh/cuttle/issues/53) and [#80](https://github.com/glim-sh/cuttle/issues/80). Target: the next stealth-Chromium release after `browser-v154.0.8037.57-1`, i.e. `browser-v154.0.8037.57-2`.

Ground rules come from [AGENTS.md](../../CLAUDE.md) and [packages/browser/README.md](../../packages/browser/README.md): KISS, the public-repo rules, the golden tripwire, the patch-series contract, and one file per patch.

## Summary and recommended sequence

Every item was checked against our own code and the 154 tree on the build box. Three results change the plan:

- **Item 11 is not a live bug.** It is a redundant flag pair, and the fix is to delete it.
- **Item 4's cause is broader than the finding says.** Ungoogled makes `disable_non_proxied_udp` the default preference for every seed.
- **Item 1's CreepJS lies have exact, cheap triggers.** Rects noise is caught by a known-geometry check whatever the seeding, so we drop it rather than seed it.

Sequence:

1. **Day 0, no rebuild.**
   - Lane T: build tooling.
   - Lane M: probes shared by realref.py and detect.py.
   - Measure the "before" state on the real Mac, the real Windows PC, bl and the Mac.
   - Three timeboxed spikes (M3).
2. **Track A, Go and image only.** Each item is a separate PR and ships through the normal release: 6, 10 (plus the 8 GB half of 8), dropping the rects-noise flag, prefs seeding (c), and 7a if its spike succeeds.
3. **Build cycle 1** (known-shape C++). Parallel lanes, one merged series, one prep, both targets.
4. **Gate.** x64 smoke on the build box; arm64 smoke and detect macos on the Mac; detect windows on bl. Compare against realref.
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
| (b) system-ui | C++ new 0063 | platform/fonts/linux/font_cache_linux.cc | M | C-display | 1 TU | 0.5d |
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
  - detect.py on 154-1: macos on the Mac, windows on bl.
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

- **A2 (Go, together with item 10):** align `windowsMachines` to the tabled device IDs, re-verifying core and memory pairings against vendor specs. Mac: one table for every Apple M entry, captured by realref on the user's real Mac and cross-checked against adryfish M2/M4. Metal limits are the same across M1-M4 at this level; confirm by diffing M2 against M4.
- **C-webgl:**
  - Extend 0016 in webgl_rendering_context_base.cc: `getParameter` numeric caps, `getShaderPrecisionFormat`, and `getSupportedExtensions` / `getExtension` hide-only (never claim an extension the backend lacks, except constant-only ones).
  - New 0058 for webgl2_rendering_context_base.cc (WebGL2 caps).
  - The table lives in a new header-only file added by 0058, keyed on the renderer string. Keep the BSD-3 notice.
- **WebGPU:** if M3 iii shows a CPU/fallback adapter, new 0060 in webgpu/gpu.cc resolves `requestAdapter` to null when a persona is active and the adapter is a fallback. The README already treats an absent adapter as a pass. Do not spoof limits.

**Validation.** Probe `webgl_caps` (a parameter list, precision formats, the sorted extensions for gl1 and gl2) and `webgpu` (null, or fallback/info/limits). realref on the real Mac and Windows PC. detect macos on the Mac, windows on bl. The expectation is equality with the real table for that GPU. posture: new `probes.webgl_caps` diff counts go to 0.

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

**Risks.** Real-time media never connects in forced mode, the same as today's zero candidates. A caller who needs media passes its own `--webrtc-ip-handling-policy` (existing override) and no `--fingerprint-webrtc-ip`.

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
- New 0064 in memory_info.cc: when a persona is active, set the pre-quantization limit to the value our binary reports on a host with 16 GB or more. Capture it raw on bl with `--enable-precise-memory-info`.
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

**Validation.** Golden diff review, plus detect windows on bl. Expect no change in CreepJS or dabi; botstop stays HUMAN.

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
- **(b) system-ui resolves to Verdana on macOS.** Rebuild; new 0063 in font_cache_linux.cc (`FontCache::SystemFontFamily`), keyed on persona. Proposed families: macOS Helvetica, matching the Dockerfile's `-apple-system` pin, so the two measure alike as they do on a real Mac; Windows "Segoe UI". Probe: the measureText width of `system-ui` against real.
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
- bl: the amd64 host running the Windows persona.
- Win PC: real Chrome on Windows.
- box: the Stage 7b x64 smoke.

| Probe | Item | Where | Ours: persona / host | Real reference | posture.json field |
|---|---|---|---|---|---|
| canvas | 1 | smoke, detect, realref | both / box (x64), Mac, bl | Mac, Win PC | creepjs.lies, probes.canvas |
| shader | 2 | detect, realref | both / Mac, bl | Mac, Win PC | are_you_a_bot.flagged, probes.shader |
| webgl_caps, webgpu | 3 | detect, realref | both / Mac, bl | Mac, Win PC (plus adryfish tables) | probes.webgl_caps, probes.webgpu |
| webrtc (+ UDP listener) | 4 | smoke, detect, realref | both / box, Mac, bl | Mac, Win PC | probes.webrtc |
| cdp getter count | 5 | smoke, detect (Runtime.enable) | both / box, Mac, bl | Mac, Win PC (value 1) | probes.cdp, are_you_a_bot.flagged |
| keyboard | 6 | detect, realref | both / Mac, bl (image) | Mac, Win PC | probes.keyboard |
| audio | 7 | smoke, detect, realref | both / box, Mac, bl | Mac, Win PC | creepjs.lies, probes.audio |
| heap | 8 | detect, realref | both / Mac (Docker VM under 16 GB), bl | Mac, Win PC | basics.heapLimitGB, probes.heap |
| css (+ availTop, system-ui) | 9, a, b | smoke, detect, realref | both / box, Mac, bl | Mac, Win PC | creepjs.likeHeadless, probes.css |
| pool | 10 | golden, detect | windows / bl | Win PC | botstop, creepjs |
| referrer, uach_headers | 11 | smoke, detect, realref | both / box, Mac, bl | Mac, Win PC | probes.referrer |
| prefs, reportingobserver | c, e | detect, realref | both / Mac, bl | Mac, Win PC | probes.prefs |

## Release and rollout

1. Track A PRs merge to main whenever they are ready: each carries its golden diff and a `## Release notes` section. Lanes T and M merge first. Nothing in Track A depends on the new binary.
2. Wait until the 154-1 builds on the box finish and are published and snapshotted. Before then, no write inside /work.
3. **Cycle 1:**
   - Sync the merged series and run `BROWSER_STAGE=prep` (T1 reverse-applies changed and removed patches, re-applies only those).
   - Build x64 and arm64 concurrently.
   - Check `sccache --show-stats`: expect roughly 35 misses plus two links per target.
4. **Gate:**
   - x64 smoke runs in Stage 7b on the box.
   - arm64 smoke on the Mac (README docker recipe).
   - detect windows on bl, detect macos on the Mac.
   - parity.py against 154-1.
   - Every posture move must head toward the realref values.
5. **Cycle 2:** items 3 and 2 (if designed), plus cycle-1 fixes. Same gate.
6. **Publish `browser-v154.0.8037.57-2`.** One commit carries:
   - `BROWSER_RELEASE_TAG` and both shas in [versions.env](../../packages/browser/versions.env), plus the Dockerfile ARG/ADD literals.
   - The binary-dependent Go changes: remove `--disable-features`, the WebRTC arg flow.
   - The golden regeneration and posture.json.
   - The README updates: canvas-noise section, Widevine, patch count and delta.
7. Build the image, run `go -C packages/cuttle run ./test/smoke`, run the real amd64 deployment gate plus the playwright-cli getter probe (item 5). Then the release PR through release-please.
