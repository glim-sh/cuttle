# packages/browser - self-hosted stealth-Chromium build pipeline

Builds cuttle's stealth-Chromium binary ourselves instead of consuming clark's
prebuilt release. A patch series over ungoogled-chromium, built with stock
depot_tools/gn/ninja on an ephemeral Hetzner box, for two targets:

- **linux/x64** - Windows persona. What cuttle ships/runs on remote hosts.
- **linux/arm64** - macOS persona. So the image runs native (no Rosetta/QEMU)
  in Docker on Apple Silicon, with a coherent `architecture: arm` UA-CH hint.

Full rationale and phase plan: `docs/plans/2607-23-self-hosted-chromium-build-pipeline.md`.

## Layout

```
patches/          forked from clark @ chromium-v148.0.7778.96-stealth5, since
                  rebased onto 151 and owned here (clark is dormant at 148)
  000-shared/     clark_fingerprint_switches.{h,cc}, clark_seed.{h,cc}, BUILD.gn.fragment
  00NN-*.patch    25 patches; applied at -F0 (see "Patch-series contract")
build/
  Dockerfile.linux  ubuntu:24.04 build image + pinned sccache
  build-linux.sh    runs in-container: sync, apply patches, gn gen, ninja, package
  run-build.sh      docker driver on the Hetzner host (persistent /work volume)
hetzner/
  cloud-init.yaml   installs docker, mounts the cache volume at /work
  provision.sh      hcloud: create volume + server (idempotent, warm-cache safe)
  teardown.sh       hcloud: delete server, KEEP volume
benches/
  detect.py         posture run: our binary vs the external detectors
  realref.py        the same measurements against REAL Chrome, for the baseline
  posture.json      committed checkpoint - the numbers the next upgrade diffs
validate/
  smoke.py          per-persona behavioral smoke (windows|macos)
  parity.py         surface diff vs our own previous release (delta report,
                    no longer a gate - see "Validate")
  report.md         (generated, untracked) delta results
versions.env        single source of version truth
```

## Build (on the Hetzner box)

```bash
# 1. Provision (Phase 1 uses the fast box for the cold build)
export HCLOUD_TOKEN=...                      # never stored in-repo
SERVER_TYPE=ccx63 packages/browser/hetzner/provision.sh
ssh root@<ip>

# 2. On the box: clone the repo onto the volume and build each target
git clone <repo> /work/repo && cd /work/repo
TARGET_CPU=x64   packages/browser/build/run-build.sh background   # Windows persona
TARGET_CPU=arm64 packages/browser/build/run-build.sh background   # macOS persona
# artifacts + shas land in /work/dist/stealth-chromium-linux-<cpu>.tar.gz

# 3. Stop compute, keep the warm cache
packages/browser/hetzner/teardown.sh
```

The `/work` volume holds the Chromium checkout, fetched toolchains,
`out/{x64,arm64}`, and the sccache cache. It persists across teardown, so later
builds are incremental (minutes). `provision.sh` never wipes a formatted volume.

**Sizing.** A full two-target tree (checkout + `out/x64` + `out/arm64` + sccache)
measures ~40 GB, so `VOLUME_SIZE` defaults to 100 GB for headroom. Size it
deliberately: cloud block volumes generally **grow online but never shrink**, so
too small is a one-command fix while too big is a bill you can only escape by
copying the data to a new volume. Keep `SCCACHE_CACHE_SIZE` below the free space
you leave it - sccache only evicts when it reaches its own cap, so a cap larger
than the volume fills the disk instead.

**Moving the cache to another volume.** Copy it as a filesystem, not as an
archive: `cp -a` (or `rsync -aHAX`) between two mounted volumes, then verify with
`rsync -aHAXn --itemize-changes <old>/ <new>/` and expect **empty** output.
`out/` staleness is decided by **mtime**, and `tar` truncates mtimes to 1-second
granularity by default while ninja records nanoseconds - a restored archive can
therefore look complete and still trigger a full rebuild of every target. Expect
the copy to be IOPS-bound rather than bandwidth-bound; the checkout is millions of
small files.

**Never run a different ninja version against an existing `out/`.** A newer ninja
prints `build log version is too old; starting over` and **rewrites `.ninja_log`**,
discarding the record that makes the tree incremental. If you want to inspect a
tree with an unknown ninja, mount it read-only first.

## Two caches, and which one actually matters

A warm rebuild is fast because of **two independent** caches. Confusing them
wastes hours:

1. **`out/<cpu>/` - ninja's built objects.** The primary incremental cache: ninja
   skips a target whose inputs are unchanged. It is invalidated by
   **mtime/command**, not content - so anything that rewrites a source file's
   mtime (a broad `git checkout -- .`, a `git clean`, a full re-apply of the
   patch series) makes ninja rebuild *everything*, even when the content is
   byte-identical.
2. **`/work/sccache` - sccache's compile cache.** Keyed on **content**
   (preprocessed source + flags). This is the safety net for case 1: if ninja
   recompiles a file whose content is unchanged, sccache returns the cached `.o`
   instantly. Without it, an mtime-invalidated rebuild is a real from-scratch
   compile.

So: keep `out/` warm by touching as little as possible, and keep sccache
**actually caching** so the times you do invalidate `out/` stay cheap.

### Making sccache actually cache

`cc_wrapper = "sccache"` in `args.gn` is necessary but **not sufficient**. By
default gn emits two flag families sccache cannot cache, and it silently marks
every such compile "non-cacheable" - the cache stays near-empty and every build
is from scratch. `build-linux.sh` disables both (only when sccache is on):

| gn arg | removes | why it's non-cacheable | safe because |
|---|---|---|---|
| `clang_use_chrome_plugins = false` | `-Xclang -add-plugin` (blink-gc, find-bad-constructs) | sccache can't hash a compiler plugin's behavior | analysis-only checks, no codegen effect; Chromium's own `cc_wrapper.gni` documents disabling it for cache users |
| `use_clang_modules = false` | `-fmodules` + `-Xclang -fmodule*` (libc++ Clang modules) | sccache hard-codes `-fmodules` as `TooHardFlag` -> `CannotCache`, and bails on unknown `-Xclang` args | `docs/modules.md` calls it experimental + not recommended, and slower cold; textual includes emit identical code |
| `use_libcxx_modules = false` | the now-unused libc++ modulemap deps | not a compile flag - a per-target dep var, NOT the gate for `-fmodules` (setting it alone was a no-op) | dropping dead deps only |

`use_clang_modules` is the `declare_args()` that actually gates the `-fmodules`
blocks in `build/config/compiler/BUILD.gn`. Chromium already force-disables
modules for `use_reclient` and `cc_wrapper == "icecc"` but not for sccache, so we
set it explicitly. `chrome_pgo_phase = 0` (PGO off) is set for the same reason - a
PGO profile makes compiles non-cacheable too.

These are build-hygiene flags, not fingerprint flags: the emitted binary behaves
identically.

```bash
sccache --show-stats   # inside the build container, or wherever SCCACHE_DIR points
```

- **Healthy:** `Non-cacheable calls` ~0; on a warm rebuild `Cache hits` is the
  large majority of `Compile requests`.
- **Broken:** a big `Non-cacheable calls` with a `Non-cacheable reasons:` list
  (e.g. `-fmodules`, `-Xclang`) - a new default flag slipped in; add the gn arg
  that removes it to the table above.

### Incremental-rebuild gotchas

- **An edited patch re-applies by itself.** Each marker is keyed on the patch's
  sha256 (`.browser-applied/<name>.<hash>.done`) and stale hashes are cleared, so
  changing a `.patch` re-applies with no manual cleanup. The tree still holds the
  previous version of that patch, though, so revert the files it touches first or
  the re-apply fuzzes.
- **Revert surgically.** To re-apply one changed patch, revert only the files it
  touches (`git checkout -- <those files>`) and clear only its marker. A
  whole-tree `git checkout -- .` reverts *every* patched file's mtime and forces
  ninja to rebuild all ~80k targets - only cheap if sccache is healthy.
- `BROWSER_NO_SCCACHE=1` opts out of sccache (and of the two flags above).

## Validate

The clark parity gate is **retired as of 151.** clark has been dormant since
2026-06-01 (last release `chromium-v148.0.7778.96-stealth5`, last commit
2026-06-22) and skipped 149, 150 and 151 entirely, so no reference tarball can
exist for any version we now ship. The gate had also become self-defeating: every
stealth improvement necessarily breaks byte-parity with a frozen 148 binary.

`validate/smoke.py` is now the pass/fail gate. `parity.py` is retained as a
**reviewed delta report** - point it at our own previous release rather than
clark's, so a bump answers "which fingerprint vectors moved between our N-1 and
our N", which is exactly what you want when reviewing a rebase.

```bash
# per-persona smoke
SMOKE_PROFILE=windows BROWSER_FONTS_DIR=/opt/personafonts \
  BROWSER_BINARY_PATH=/work/build/src/out/x64/chrome \
  python3 packages/browser/validate/smoke.py
SMOKE_PROFILE=macos \
  BROWSER_BINARY_PATH=/path/to/arm64/chrome \
  python3 packages/browser/validate/smoke.py   # runs on an arm64 host

# delta against our own previous release (downloads and sha-verifies it)
BROWSER_BINARY_PATH=/path/to/new/chrome \
  python3 packages/browser/validate/parity.py
```

**Both runs need a display** - see "A display is required" below; without one the
WebGL vector reads as two empty strings and the smoke reports a broken GPU spoof.

The build host packages arm64 **ungated** for a different reason: it cannot
execute a cross-built binary at all. Gate that artifact on an arm64 machine
instead. The published runtime image supplies the macOS font pack and a python3;
it has no pip, so unpack the pure-python `websocket-client` wheel and mount it:

```bash
docker run --rm --platform linux/arm64 \
  -v <extracted-build>:/opt/browser-new:ro -v <wheel-dir>:/pylibs:ro \
  -v packages/browser/validate:/work/packages/browser/validate:ro \
  -v packages/browser/versions.env:/work/packages/browser/versions.env:ro \
  -e PYTHONPATH=/pylibs -e BROWSER_BINARY_PATH=/opt/browser-new/chrome \
  -e BROWSER_FONTS_DIR=/opt/personafonts \
  --entrypoint bash ghcr.io/glim-sh/cuttle:latest -c \
  'Xvfb :99 -screen 0 1440x900x24 & export DISPLAY=:99
   python3 /work/packages/browser/validate/smoke.py'
```

`parity.py` writes its report to `validate/report.md` by default, or wherever
`PARITY_REPORT` points. That file is generated output and is not tracked.

Both targets are now validated by internal coherence (macOS-persona smoke:
`architecture == "arm"`, frozen `Intel Mac OS X 10_15_7` UA, no `HeadlessChrome`
token, single-source brand version), plus the surface delta against our own
previous release.

## Verifying the stealth identity

What a healthy identity looks like, and the gotchas that look alarming but are
not. `validate/smoke.py` automates the per-persona coherence checks and
`test/smoke` (`go run ./test/smoke`) covers per-seed isolation; this section is
what to check by hand against a running seed.

Point any CDP client at a seed and evaluate in a page. Values are seed-derived,
so exact strings vary; what matters is that each is *coherent* with the platform
the seed claims.

| Surface | Good | Bad (investigate) |
|---|---|---|
| `navigator.webdriver` | `false` | `true` |
| `navigator.platform` | matches the UA platform (`Win32`) | mismatched (e.g. `Linux` under a Windows UA) |
| `navigator.userAgent` | the persona UA, no `HeadlessChrome` token | a `HeadlessChrome` token anywhere |
| WebGL renderer | a real desktop GPU string via **ANGLE / Direct3D11** | contains `SwiftShader`, `llvmpipe`, or `Mesa` |
| WebRTC ICE candidates | only the proxy exit IP, or none | a private/LAN IP (`10.*`, `192.168.*`, `172.16-31.*`) or the host IP |
| WebGPU (`navigator.gpu`) | absent, or an adapter matching the WebGL GPU | an adapter that contradicts the WebGL GPU |
| `speechSynthesis.getVoices()` | non-empty, with entries where `localService === false` and `voiceURI` starts with `Google` | **empty**, or no remote entries - a documented "this is not real Chrome" test |
| Worker vs main thread | `userAgent` and the unmasked WebGL vendor/renderer identical in both | any disagreement - spoofing only the main thread is the classic miss |

The series spoofs the WebGL GPU strings from a pool of real desktop GPUs, so the
renderer reads as a genuine ANGLE/Direct3D11 adapter **even though the host has
no GPU**. If WebGL reports `SwiftShader`/`llvmpipe`, the spoof is not engaging -
that is a real regression.

### Probe from a secure context, or the result is confident nonsense

Several high-value surfaces are gated on a **secure context** and do not exist on
`about:blank` or a plain `http://` page:

| API | On a non-secure page |
|---|---|
| `navigator.requestMediaKeySystemAccess` (EME/Widevine) | `TypeError` on every call |
| `navigator.mediaDevices` | **`undefined`** - so `enumerateDevices()` throws |
| `navigator.mediaCapabilities.decodingInfo` with a key system | `SecurityError` |
| `navigator.bluetooth`, `.usb`, `.serial`, `.hid` | **all absent** - indistinguishable from an unsupported build |
| `navigator.userAgentData` | **`undefined`** - every UA-CH probe reads as missing |

Each failure mode is indistinguishable from "the feature is missing", which is
exactly the conclusion a probe reaches. This has produced a false "no Widevine at
all" reading, a false "mediaDevices is broken" reading, and a false "all four
device APIs are missing" reading, on builds where every one was fine. The last
nearly sent a fix after three surfaces that were never broken - the real gap was
`navigator.bluetooth` alone, visible only from a secure context.

Probe over `https://` or `http://127.0.0.1` (localhost counts as secure).

### A display is required, or the probe lies to you

Without one, WebGL returns two empty strings, which reads exactly like a broken
GPU spoof. `Xvfb :99 -screen 0 1440x900x24 & export DISPLAY=:99`. A previously
shipped binary reproducing the same empty result is the check that tells you it
is the harness, not the build - run that control before believing a WebGL
failure.

### Chrome log lines that are benign

| Log line | Why it's harmless |
|---|---|
| `Failed to connect to the bus` (dbus) | No system D-Bus in a container; Chrome logs and continues. |
| `Failed to adjust OOM score of renderer ... Permission denied` | Needs a capability the container doesn't grant; cosmetic. |
| `Failed to decode OID` (`ev_root_ca_metadata`) | Cert-metadata warning, unrelated to stealth. |
| `vkCreateInstance: Found no drivers` / `VK_ERROR_INCOMPATIBLE_DRIVER` | No Vulkan GPU on the host; expected. |
| `Automatic fallback to software WebGL ... --enable-unsafe-swiftshader` | See below - do **not** act on this one. |
| `GPU stall due to ReadPixels` | Performance note, not a correctness issue. |

### Do not add `--enable-unsafe-swiftshader`

Chrome nudges you toward it when WebGL falls back to software rendering.
**Passing it is a stealth regression, not a fix.** It forces the SwiftShader
software renderer, and a raw `SwiftShader`/`llvmpipe` WebGL string is a
well-known automation tell. The series instead spoofs a real GPU string on top of
whatever renders underneath. `--ignore-gpu-blocklist` (already in the base args)
is what lets WebGL work at all under software rendering; the patches make it
*look* real.

### Canvas noise is detectable, and kept on purpose

CreepJS names our noise directly - `CanvasRenderingContext2D.getImageData`
"pixel data modified", `measureText` "metric noise detected",
`Element.getClientRects` "unknown rotate dimensions". That is accurate: the
`--fingerprinting-*-noise` switches perturb those surfaces, and a detector
comparing against a known-good render can see it. Real Chrome reports no lies.

It stays on, and the reasoning matters more than the conclusion. The noise is
what makes each seed's canvas unique. Remove it and every seed sharing the same
font pack and GPU string renders a **byte-identical** canvas, because the spoof
is at the string level while rasterisation is the same software path on the same
host - so the seeds become trivially correlatable as one operator. Trading "this
canvas was modified" for "these thousand browsers are the same machine" is a bad
trade.

The genuine fix is neither: make the canvas differ because the *machine* differs.
That needs per-seed divergence in the rasterisation stack itself, not a
post-hoc perturbation, and it is a much larger project than the switch. Until
then this is a known, deliberate cost - do not "fix" it by dropping the flags.

### An exposed API must be furnished, or absence would have been safer

Patch #52 (speechSynthesis) and #53 (BarcodeDetector) exist for the same reason:
an interface that is *present but hollow* is a stronger tell than one that is
absent. Plenty of real browsers lack a given API, so absence is a plausible
platform. "Interface exposed, returns nothing" is a configuration that ships
nowhere on earth.

BarcodeDetector is enabled on the macOS persona only, because it is the single
feature separating CreepJS's Windows and Mac platform estimates - the other
discriminator, `hasTouch`, is a tautology on the Windows side. Real Chrome ships
it on Mac/Android/ChromeOS and nowhere else, so the Windows persona must not get
it; that was measured on real Windows hardware, not assumed.

Enabling it created the obligation. The Linux container has no shape-detection
backend, so the mojo pipe disconnects and the spec's error paths yield an empty
format list and a `detect()` that rejects with `NotSupportedError`. Patch #53
supplies macOS's real Vision format list (11 entries, ordered by the mojom
`BarcodeFormat` enum ordinal, which is how `base::flat_set` returns them) and
resolves `detect()` with `[]` - what a real Mac returns for a barcode-free image.

Chromium's own ZXing-backed provider is not an option:
`services/shape_detection/features.gni` gates it on `is_chrome_branded`, i.e. the
proprietary Barhopper blob.

Two known limits, accepted deliberately: a probe supplying a *known* barcode
image would see `[]` where a real Mac decodes it, and `detect()` returns in ~0ms
where real Vision takes measurable time. Neither is read by anything observed in
the wild - of 20 deobfuscated commercial detector scripts (Akamai, DataDome,
PerimeterX, Forter, WhiteOps, Sift, ThreatMetrix, FingerprintJS, Arkose, Distil,
BotGuard) none reference the API at all, and CreepJS tests existence only.

### Spoofed values must reach CSS, not just JS

Patches #11 and #14 spoof `screen.*` and `window.devicePixelRatio`, but CSS media
evaluation kept reading the real display, so two lines caught the spoof:

```js
matchMedia(`(device-width: ${screen.width}px) and
            (device-height: ${screen.height}px)`).matches   // was false
matchMedia(`(resolution: ${devicePixelRatio}dppx)`).matches // was false on macOS
```

CreepJS runs exactly these and reported them as `Screen: failed matchMedia` (both
personas) and `Window.devicePixelRatio: lied dpr` (macOS only - the Windows
persona claims DPR 1, which already matched). Patch #54 routes the three
`MediaValues::Calculate*` helpers through the same `clark::seed` source the JS
getters use, which covers every media-query path because `MediaValuesDynamic` and
`MediaValuesCached` both funnel through them.

It also patches `Document::DevicePixelRatio()`, so srcset / `image-set()`
selection, the DPR and Sec-CH-DPR client hints and the resource-width hint agree
with the number JS reports. Patching `MediaValues` alone would have made the
preload scanner pick the 2x candidate while `HTMLImageElement` picked 1x - a
double fetch and a console warning, a new tell created by the fix.

`LocalFrame::DevicePixelRatio()` is deliberately NOT patched. It also converts
IME bounds, autoscroll positions and highlight hit points between CSS and device
pixels; lying there puts CDP input at half coordinates. The two seams that *are*
patched cover every value that is reported or selected on, and none that geometry
or rasterisation depends on. Layout does move in two places on the macOS persona,
both toward what a real retina Mac does: `<input type=date>` sub-field width and
fenced-frame frozen size. Canvas is unaffected - `Document::DevicePixelRatio()`
reaches it only for the broken-canvas icon.

### Challenge cold-clear depends on the exit IP, not the fingerprint

Whether a seed clears an escalated anti-bot challenge is dominated by the
**reputation of that seed's proxy exit IP**, not by the browser fingerprint. A
coherent identity on a clean exit clears in seconds; the same identity on a
flagged exit will not clear no matter how many reloads. For a CDP client: on a
*persistent* challenge, rotate to a fresh seed (which draws a new exit) rather
than retrying. Budget one cheap same-exit retry for a transient challenge, then
rotate.

## Upgrade and release workflow

The whole point of cuttle: collapse a multi-upstream hand-reconciliation into one
procedure. This is that procedure - follow it top to bottom for any Chromium bump
or patch-series change. Budget ~80 GB disk and ~32 GB RAM on the build host; a
warm cache volume keeps a rebuild to minutes.

1. **Pin the new engine.** Set `CHROMIUM_VERSION` and `UC_TAG` in `versions.env`
   to the new ungoogled tag. `CHROMIUM_VERSION` is the single source of every
   version string cuttle emits - the validate harness reads it directly and
   `internal/fingerprint` keeps one matching literal, guarded by
   `TestChromiumVersionPin`.

2. **Rebase the patch series** in `patches/` onto the new tag, fixing drift patch
   by patch. The build hard-fails on a patch that does not apply. Read the
   patch-series contract below first - a clean apply is not evidence the result
   compiles, let alone behaves.

3. **Build both targets** (see Build above). x64 is gated on the build host by
   `validate/smoke.py` before it is packaged; arm64 cannot be executed there at
   all, so it is packaged ungated and **must** be gated separately on an arm64
   machine before release.

4. **Gate both personas.** `validate/smoke.py` per persona, with a display up. A
   binary that has not passed its own persona's smoke does not get published.

5. **Review the surface delta.** `validate/parity.py` against the previous
   release. Every diff is either version-derived (the harness excuses those and
   says so) or a change someone must consciously accept. Treat a surviving diff
   as a review item, not a failure to route around.

6. **Reconcile the Go side if the flag dialect moved.** Does the new binary still
   honour the `--fingerprint-*` flags `cuttle serve` emits? Watch for new CDP
   quirks (Chrome 148 shipped an empty service_worker `browserContextId`). The
   load-bearing pieces are `internal/serve/wsproxy.go` and
   `internal/fingerprint/args.go`; any argv/proxy/geoip change must land as a
   reviewed `internal/fingerprint/testdata/golden.json` diff (`just parity-golden`).

7. **Run the external detectors by hand and record the result.** These are not
   gates - they are third-party pages that change without notice, and CreepJS in
   particular removed its trust score entirely, so there is no stable number to
   assert.

   `benches/detect.py <windows|macos>` runs all of them in one pass and prints a
   posture summary. It composes its flag set from `golden.json` exactly as the
   daemon does, so it cannot measure a browser we do not ship - hand-written flag
   sets produced three false findings during the 151 rebase before this existed.
   Each external section is isolated, so a page being down or restructured
   degrades one line instead of failing the run, and assertions are kept separate
   from scores that drift.

   **Run it once per persona, each on its matching arch.** The persona is
   arch-locked - the arm64 build reports `architecture: arm` - so pointing the
   macOS persona at the x64 binary measures a machine that does not exist. In
   practice: the Windows run on the amd64 build host, the macOS run on an arm64
   machine, same split as the smoke.

   ```bash
   BROWSER_BINARY_PATH=/path/to/chrome DISPLAY=:99 \
     python3 packages/browser/benches/detect.py windows --json ours-windows.json

   # the reference half: real Chrome, run ON a real Mac and a real Windows box
   python3 packages/browser/benches/realref.py --json real-windows.json
   ```

   Four measurements - real and ours, on each platform - combine into
   `benches/posture.json`, which is committed. That file is the checkpoint: the
   next upgrade diffs its new numbers against it to see what moved, and that diff
   goes in the release notes. A score with nothing to compare it against is not a
   result - it took a real-Chrome baseline to discover that our macOS persona was
   scoring as Windows.

   What each target is for:
   - [CreepJS](https://abrahamjuliot.github.io/creepjs/) - read
     `window.Fingerprint` rather than the page. The trust score, the lies panel
     and the crowd-blending comparison were all deleted upstream in 2025, so any
     "CreepJS grade" claim dated after that is describing a page that no longer
     renders; the object still carries the data. Record `headless` and `stealth`
     (raw booleans, 3 and 5 keys) and **`platformEstimate`**, which scores
     Windows/Mac/Linux from fonts and feature detection and is therefore a direct
     read on whether the persona font pack is working. Weigh `likeHeadless` against a
     real browser rather than against zero - a genuine headless Chrome on a real
     Mac fires 5 of those keys, including `noTaskbar` and `noContentIndex`. We
     fire 6, and only two of them are ours: `prefersLightColor` and `noWebShare`.
     We actually *pass* `noTaskbar` (`--fingerprint-taskbar-height` makes
     `availHeight` differ from `height`) and `hasSwiftShader` (the worker-scope
     renderer is spoofed), both of which a real headless Mac fails.
     **Only `abrahamjuliot.github.io` is the real thing** - upstream's README
     flags the `creepjs.org` / `creepjs.com` style domains as malicious mirrors.
   - [deviceandbrowserinfo.com/are_you_a_bot](https://deviceandbrowserinfo.com/are_you_a_bot)
     - the only public page that names CDP. Returns JSON with
     `isAutomatedWithCDP`, `isAutomatedWithCDPInWebWorker`, `isWebGLInconsistent`
     and `isHeadlessChrome`; the
     [interactions variant](https://deviceandbrowserinfo.com/are_you_a_bot_interactions)
     adds `hasCDPMouseLeak`. That last one is live against real Chromium and is
     the one that matters here, because cuttle dispatches input over raw CDP.
   - [rebrowser-bot-detector](https://bot-detector.rebrowser.net/) - driver-layer
     leaks, not persona plausibility. Its probes must be triggered from the
     driver; results land in `#detections-json`.
   - [botstop.io](https://botstop.io/) - a live production detector used as a
     pass/fail oracle. It deliberately shows no signal breakdown, so it tells you
     *whether*, never *why*.

   Weak targets, kept only for continuity: `bot.sannysoft.com` is the 2018 Intoli
   and fpscanner checks unchanged, so passing it means you did not fail 2018;
   `bot.incolumitas.com` self-reports v0.6.3 (June 2024) and its author moved on
   to botstop; `browserscan.net` and `pixelscan.net` carry paid placements for the
   antidetect browsers they grade, and at least one of their checks moves on a
   single flag. Do not treat a green score from those as evidence.

8. **Publish the browser release.** Tag `browser-v<CHROMIUM_VERSION>-<N>`, both
   tarballs, and the verification block from steps 4-7 in the body. Then pin it:
   `BROWSER_RELEASE_TAG` + both `BROWSER_SHA256_*` in `versions.env`, and the
   matching `ARG BROWSER_TAG` / `ADD --checksum` literals in
   `ops/docker/Dockerfile`. The Dockerfile is what the image actually pulls, so
   the two must agree - `TestDockerfilePinsMatchVersionsEnv` and
   `TestReleaseTagMatchesChromiumVersion` enforce both halves. Do all of this in
   one commit: a persona whose version disagrees with its own binary is the exact
   split this pipeline exists to prevent.

9. **Build the image and validate in two layers.** They cover different risks:
   - **`test/smoke` (`go run ./test/smoke`, fast, local).** Confirms the binary
     still applies fingerprints, isolates seeds (distinct canvas), looks stealthy,
     and connects cleanly under cold cycling. It is client-agnostic raw CDP, so it
     **cannot** observe a CDP quirk that crashes a playwright client.
   - **Real amd64 deployment (the gate).** The actual playwright-core consumer
     path against live sites on a real amd64 host. Only this surfaces a
     playwright-crashing CDP quirk and confirms real challenge clears. The local
     arm64 image is a different persona, fine for a smoke but never the gate.

10. **Publish the image.** A `vX.Y.Z` release cuts `ghcr.io/glim-sh/cuttle` (see
    `docs/RELEASING.md`), then bump the consumed digest wherever cuttle is deployed.

## Patch-series contract

We forked clark's patch series and now own it. We do NOT continuously re-pull.

**Our series applies at `-F0` (zero fuzz); ungoogled's applies at `-F3`.** That
split is deliberate and load-bearing. On the 148 -> 151 rebase, `-F3` let
`0013-window-outer-and-dpr` apply with a green exit while landing its hunks in
the *wrong functions* (`outerHeight`'s body inside `confirm()`, a `return int`
inside `prompt()`), because 151 braced the `if (!GetFrame())` guards and killed
the leading context. `outerWidth`/`outerHeight` would have shipped unpatched with
the build reporting success. Three ungoogled patches genuinely need fuzz and are
upstream-authored for the exact tag, so they keep `-F3`; ours must never.

**Do not hand-write hunk headers.** `patch` rejects on counts, not content, and
context arithmetic is easy to get wrong three times in a row. Generate hunks
mechanically: apply the intended edit to a copy of the target file, then
`diff -u orig new`. That is how `0013`, `0048` and `0049` were rebased onto 151.

**`-F0` proves nothing about a context-free hunk.** A hunk written
`@@ -192,0 +197,94 @@` carries no context lines at all, so `patch` inserts it
blindly at the recorded line number and it applies cleanly at *any* fuzz level,
however far the file has moved. On the 151 rebase this put `0016`'s GPU-pool
helpers inside the body of a multi-line macro; the apply gate was green and the
compiler caught it. Audit for them before trusting a clean apply:

```sh
grep -cE '^@@ -[0-9]+,0 \+[0-9]+' patches/0*.patch
```

Any patch with a non-zero count is unverified by the gate no matter what it
reports. Regenerate it with real context via `diff -u`, then confirm the result
by **compiling**, not by re-applying.

**After regenerating a patch, diff its added identifiers against the original.**
Rebuilding `0016` from a reverted base silently dropped one hunk - the whole
`UNMASKED_VENDOR/RENDERER` spoof - which would have shipped a single space as the
WebGL renderer string. Nothing failed; a `-Wunused-function` warning on the now
unreferenced helper was the only signal. Compare the sets and read the build log
for unused-symbol warnings.

**Re-applying after a failure needs a full tree reset**, otherwise the partially
applied patch makes `--forward` report "previously applied" and skip every hunk.
Stage 3 does this: `git reset --hard`, then `git submodule foreach` reset (three
ungoogled patches edit files inside the `v8` and `third_party/devtools-frontend`
submodules, which a top-level reset does not reach), then `git clean -fd -e
uc_staging` (never `-x`, which would delete ~19 GB of gclient-managed
`third_party`).

**Switches added by the series.** Beyond clark's `--fingerprint-*` set, patch 0052
adds `--fingerprint-voices`. It is an opt-*out*: the persona voice list is on by
default and is disabled with `--fingerprint-voices=false` (or `=0`). The value is
required - a bare `--fingerprint-voices` reads as an empty string, which is
neither, so the list stays on.

**stealth5 delta.** The series was forked from clark's stealth5 (24 patches) and
is now 25: `0027-analyser-node-noise` was cherry-picked during the 151 rebase,
once retiring the parity gate removed the reason not to; `0041-chrome-stealth-defaults`
was dropped (see the build-pipeline plan, L2); and `0052-speech-synthesis-persona-voices`
is cuttle-authored rather than inherited, hence its `Cuttle*` symbols. `0047-suppress-cdc-globals`
was evaluated and **deliberately not taken** - it registers its V8 extension in
`headless/lib/renderer/headless_content_renderer_client.cc`, and we run headed
(Xvfb + openbox), so it never executes. The `cdc_` globals it strips are
chromedriver artifacts anyway and we drive raw CDP. `0002-headless-window-chrome`
and `0045-headless-user-agent` live in the same headless embedder files and are
likely dead for the same reason - probing confirmed nothing observable
distinguishes them from the natural headed path, so verify before spending
rebase effort on them.

**ungoogled's `flags.gn` is merged into `args.gn`, not ignored.** Its patch
series is authored against those flags: `fix-building-without-mdns-and-service-discovery.patch`
strips `service_discovery_client_` from `dns_sd_registry.h`, so building with
`enable_service_discovery` at its default `true` fails ~30k targets in. Writing
`args.gn` from scratch also silently shipped default Google API keys and
field-trial testing config for the whole life of the pipeline before 151.

## macOS-persona fonts (arm64 image)

Baked into the image at `/opt/personafonts` by the `personafonts-arm64` stage,
the same way the Windows pack is built for amd64: free fonts with their `name`
table rewritten to the macOS family names, and - for the families with no
metric-compatible free equivalent - Apple's advance widths and vertical metrics
stamped on from `ops/docker/macfonts/metrics.json`. Renaming alone is not
enough: a detector compares `measureText` widths against the CSS generics, so a
renamed font keeping its own metrics still reads as a substitute.

No Apple font software is redistributed. The metrics table holds integers only
(advance widths, hhea/OS-2), which is the basis on which Liberation and Nimbus
were built; regenerate it on a Mac with `scripts/extract-font-metrics.py`.

Deliberately NOT faked: SF Pro, SF Mono, New York. Stock macOS exposes those
only as hidden `.SFNS-*` system faces, so a real Mac answers "absent" when a
page probes for them - shipping them would create a tell rather than remove one.
`-apple-system` is pinned to Helvetica in `60-macfonts-system-ui.conf`.

Because the pack is baked, every host presents the same macOS font surface -
Apple Silicon, Linux arm64 and CI alike. No host bind-mount, no Docker
file-sharing prerequisite. Only the pack matching the image's arch is copied in,
so neither image carries the other persona's fonts.

## Widevine / EME

amd64: enable via a separate shipping-args overlay + sideload Google's linux-x64
CDM (after parity is recorded). arm64: deferred research spike -
`docs/plans/2607-23-arm64-widevine-spike.md`.
