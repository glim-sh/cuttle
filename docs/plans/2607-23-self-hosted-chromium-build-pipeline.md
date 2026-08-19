# Self-hosted stealth-Chromium build pipeline

Status: **implemented + shipped.** Branch: `feat/chromium-build-pipeline`. Author
date: 2026-07-23.

## Why

cuttle today consumes a **prebuilt** clark-browser release tarball
(`ops/docker/Dockerfile`, `CLARK_TAG=chromium-v148.0.7778.96-stealth5`,
pinned by sha256). We own nothing: if clark stops publishing, or we need an arch
they don't ship, or we want a patch they won't take, we are stuck.

This plan stands up **our own build pipeline** that reproduces clark's method (a
patch series over ungoogled-chromium, built with stock depot_tools/gn/ninja) so
we can build the stealth-Chromium binary ourselves, on demand, on Hetzner, for
both arches we care about:

- **linux/amd64** - Windows persona. What cuttle ships and runs on remote hosts
  today. First target; must reach validated parity with clark's own amd64
  binary before anything else.
- **linux/arm64** - macOS persona. So the cuttle image runs **natively** (no
  Rosetta/QEMU emulation tax) inside Docker on an Apple-Silicon MacBook, with
  novnc, and a *coherent* fingerprint (a real arm64 host presenting as an
  Apple-Silicon Mac; the UA-CH `architecture` hint genuinely is `arm`).

Clark's patches are MIT and clean-room (`METHODOLOGY.md`), so vendoring and
maintaining them ourselves is legally clean. When clark disappears we re-diff the
series against the next ungoogled tag and rebuild.

## Key facts that shape the plan (verified against the clark source)

These are load-bearing; do not re-derive them cold.

1. **clark is NOT a Chromium fork.** It is 26 `.patch` files + a
   `patches/000-shared/` dir (4 hand-authored C++ files:
   `clark_fingerprint_switches.{h,cc}`, `clark_seed.{h,cc}`, plus a
   `BUILD.gn.fragment`) applied on top of **ungoogled-chromium tag
   `148.0.7778.96-1`**. Total ~192 KB.

2. **The authoritative build is `build/build-linux.sh` (~650 lines).** It runs
   inside an `ubuntu:24.04` container (`build/Dockerfile.linux`), driven by
   `build/run-linux-build.sh` (Docker + a persistent named volume so partial
   builds resume). It does: clone ungoogled @ pinned tag -> `clone.py` (gclient
   wrapper) to fetch Chromium src -> gclient-sync recovery -> apply ungoogled
   series (`patch -p1 --batch --forward -F3`, skippable) -> apply clark series
   (hard-fail if a real diff patch fails) -> copy 000-shared C++ into
   `third_party/blink/common/` and wire into `BUILD.gn` -> write `args.gn` ->
   fetch only the toolchains gn/ninja need (rust, clang, node, gperf, gn via
   cipd) -> hand-stub the files `gclient runhooks` would generate
   (`gclient_args.gni`, `LASTCHANGE`, `DAWN_VERSION`, `skia_commit_hash.h`,
   `gpu_lists_version.h`) -> `gn gen` -> `ninja -C out/Default -j$(nproc)
   chrome` -> package tar.gz -> in-container smoke test.

3. **The script already cross-compiles arm64-HOST -> x64-TARGET** (for building
   on Apple Silicon ~3-5x faster than Rosetta). But **every build produces an
   x64 target.** There is no `target_cpu="arm64"` lane. Producing a **linux/arm64
   target binary is genuinely new work** - that is Phase 3, not a config flip.

4. **`args.gn` deliberately disables the slow/heavy paths:** `is_official_build =
   true` but `use_thin_lto = false`, `is_cfi = false`, `chrome_pgo_phase = 0`,
   `safe_browsing_mode = 0`, `symbol_level = 0`. LTO/PGO/CFI off is why a build is
   ~2-6h not ~10h+, and why no PGO profiles need fetching. **We keep these
   exactly** for parity with clark's published binary.

5. **There is NO build cache in clark's setup** (no sccache/reclient). Its "warm"
   state is purely the **persistent `/work` volume**: the Chromium checkout
   (~80 GB), fetched toolchains, and a populated `out/` for incremental ninja.
   Re-running the same container against the same volume resumes. Our Phase 1
   warm-cache goal maps directly onto: materialize that volume once, then reuse
   it. (We MAY add `cc_wrapper="sccache"` as an enhancement - see Phase 1 notes -
   but the volume alone already gives incremental rebuilds in minutes.)

6. **Byte-for-byte reproducibility is impossible and is NOT the validation
   target.** `build-linux.sh` stubs `LASTCHANGE` with `date +...` and
   `skia/gpu` commit-hash headers with all-zeros; timestamps and build paths make
   the output non-deterministic. "Validate towards clark's binary" therefore
   means **behavioral / fingerprint-surface parity**, not a matching hash. See
   the Validation section.

7. **clark's own smoke test (`tests/linux_smoke.py`) is a ready-made behavioral
   validator** - drives the binary over CDP and asserts the full JS/UA-CH/WebGL/
   canvas/audio surface. Critically it asserts **`architecture == "x86"` and
   `bitness == "64"`** (lines ~463) under Windows/Linux personas. **The arm64 /
   macOS persona must flip that expectation to `architecture == "arm"`** - so the
   validator needs a per-arch/persona expectation set, it is not reusable
   verbatim for arm64.

8. **cuttle's launch contract is arch-agnostic and needs no Go changes to swap
   the binary:** `internal/fingerprint/binary.go` resolves `CUTTLE_BROWSER_BINARY`
   to a path; `internal/serve/pool.go` execs it. The Windows persona lives in
   `internal/fingerprint/args.go` `ForkParityArgs` (currently hardcoded Windows);
   the macOS persona is fully spec'd and wire-proven in
   `docs/2607-17-native-macos-backend.md` (frozen `Intel Mac OS X 10_15_7` UA,
   `--fingerprint-platform=macos`, UA/CH single-source pinning to fix clark's
   two-code-path leak). The arch lock is only in the Dockerfile (`GOARCH=amd64` +
   x64 `COPY`), `Justfile` (`--platform linux/amd64`), Helm (arch selector), and
   `internal/backend/local.go` (forces `--platform linux/amd64`).

## Target layout: `packages/browser/`

All build-related code and docs live here (per the directive; this is a distinct
lifecycle from the Go daemon, so it earns its own top-level dir).

```
packages/browser/
  README.md                 # what this is, how to build, how to validate, how to bump versions
  versions.env              # single source of version truth (see below)
  patches/                  # copied VERBATIM from clark @ the pinned release
    000-shared/             #   clark_fingerprint_switches.{h,cc}, clark_seed.{h,cc}, BUILD.gn.fragment
    0001-*.patch ... 0051-*.patch
  build/
    Dockerfile.linux        # ubuntu:24.04 build image (from clark, verbatim first)
    build-linux.sh          # adapted from clark: parametrized by TARGET_CPU=x64|arm64
    run-build.sh            # driver: docker + volume, TARGET arg, out/<arch> dirs
  hetzner/
    cloud-init.yaml         # installs docker, mounts the volume at /work
    provision.sh            # hcloud: create volume + server, wait, print ssh
    teardown.sh             # hcloud: delete server, KEEP volume
  validate/
    parity.py               # behavioral parity harness: our binary vs clark's tarball
    smoke.py                # from clark tests/linux_smoke.py, per-arch/persona expectations
    report.md               # (generated) parity results, checked in per release
```

`versions.env` (sourced by every script, the only place versions live):
```
CHROMIUM_VERSION=148.0.7778.96
UC_TAG=148.0.7778.96-1                     # ungoogled-chromium git tag
CLARK_REF_TAG=chromium-v148.0.7778.96-stealth5   # clark release we validate against
CLARK_REF_ASSET=clark-browser-linux-x64.tar.gz
CLARK_REF_SHA256=30cca952d11d94ca3424ac184b100c88ba686bfb87f2aaf4668ac5767562bd67
```

## Phase 0 - prerequisites (do first, once)

- **Hetzner API token + hcloud context (deferred to Phase 1 start, Q7).** `hcloud`
  is installed (v1.66.0) but has **no active context** (`hcloud context list` shows
  none). The token is created + `hcloud context create cuttle-build` run at the
  moment Phase 1 is implemented, not now. **Location: no preference** - default to
  whatever has CCX capacity (`nbg1`/`fsn1`); `provision.sh` takes `LOCATION`.
  Every Phase 1 command assumes the context is active.
- **SSH key in the project:** `hcloud ssh-key create --name cuttle-build --public-key-from-file ~/.ssh/id_ed25519.pub` (or reuse an existing one). Needed so `provision.sh` can attach it.
- **Build box size (DECIDED).** Cross-compiling both targets from one amd64 box.
  Chromium wants 32 GB+ RAM (64 GB comfortable at link), lots of NVMe, many cores.
  Two configs, both CCX (dedicated-vCPU AMD EPYC, hourly-billed, destroy after
  use):
  - **Warm-up (Phase 1 cold build): `ccx63`** (48 vCPU / 192 GB) - fastest cold
    build, minimizes the one-time ~1h clone + hours of compile.
  - **Future builds (Phase 2+ incremental): `ccx53`** (32 vCPU / 128 GB) -
    cheaper; incrementals are minutes, so cores barely matter.
  `provision.sh` takes `SERVER_TYPE` (default `ccx53`); Phase 1 runs it once with
  `SERVER_TYPE=ccx63`. Confirm live pricing at checkout; Hetzner moved prices
  repeatedly in 2026.
- **Volume size (DECIDED): 500 GB.** Must hold shared src (~80-100 GB) +
  `out/amd64` (~30 GB) + `out/arm64` (~30 GB) + toolchains + **the sccache cache
  for both targets** (capped, see Q3/sccache below) + headroom. 500 GB keeps us
  clear of ENOSPC mid-build (a Chromium build failing on disk at hour 3 is the
  worst failure mode). ~22 EUR/mo standing cost; volumes persist independently of
  the server. Cache lives on this same volume.
- **sccache (DECIDED).** Use `cc_wrapper="sccache"` (sccache, not ccache -
  sccache is the wrapper Chromium officially added support for and handles the
  cross-compile toolchains cleanly; ccache also works but sccache is the trodden
  path here, same tool family you use for Rust). Cache dir on the mounted volume:
  `SCCACHE_DIR=/work/sccache`, `SCCACHE_CACHE_SIZE=150G` (both targets + a version-bump generation,
  capped so it can't eat the volume). This survives a `git clean` of the src tree
  and makes cross-target + post-bump rebuilds fast. Added from Phase 1 (not
  deferred) since we want the warm cache populated during the initial build.

## Phase 1 - Hetzner build box + volume + warm cache

**Goal: one-time materialization of the persistent build state, then stop paying
for compute.** After this phase the 500 GB Volume holds a fully-synced Chromium
checkout, all fetched toolchains, and (ideally) one completed build per target so
later phases get incremental (minutes) rebuilds instead of the ~1h clone + hours
of first compile.

Steps:

1. `packages/browser/hetzner/provision.sh`:
   - `hcloud volume create --name cuttle-build-cache --size 500 --location <loc>`
   - `hcloud server create --name cuttle-builder --type ${SERVER_TYPE:-ccx53}
     --image ubuntu-24.04 --ssh-key cuttle-build --volume cuttle-build-cache
     --user-data-from-file cloud-init.yaml` (Phase 1 invokes with
     `SERVER_TYPE=ccx63` for the fast cold build).
   - `cloud-init.yaml`: install docker.io; `mkfs.ext4` the volume **only if
     unformatted** (guard on `blkid`, so re-provisioning against an existing warm
     volume never wipes it); mount it at `/work`; add fstab entry.
   - print the server IP + `ssh root@<ip>`.
2. On the box, build the image and run the first build for **amd64** target
   against `/work` (this does the expensive clone + gclient sync + toolchain fetch
   + full compile). Use `run-build.sh` with `TARGET_CPU=x64`, output to
   `/work/out/amd64`. Expect ~1h checkout + ~2-4h compile on CCX53.
3. Run the first build for **arm64** target (`TARGET_CPU=arm64`, `/work/out/arm64`)
   reusing the same `/work/build/src` checkout - only the compile is new, the
   ~80 GB source and most toolchains are shared. (This also proves the Phase 3
   arm64 lane early; if arm64 needs patch deltas, discover them here.)
4. **sccache is on from this first build** (`cc_wrapper="sccache"`,
   `SCCACHE_DIR=/work/sccache`, `SCCACHE_CACHE_SIZE=150G`) so the initial compile
   populates the cache we carry forward. Verify cache-hit rate on the second
   target's build (`sccache --show-stats`) - the shared translation units should
   already hit.
5. **Stop compute, keep cache:** `packages/browser/hetzner/teardown.sh` runs
   `hcloud server delete cuttle-builder` and **leaves the volume**. (Powering off
   still bills on Hetzner; deleting the server is the real "stop." The warm state
   is on the volume, not the server root disk - that is why the checkout MUST live
   under the mounted `/work`.) Optionally `hcloud image create` a snapshot of the
   configured server to skip re-installing docker next time (cheap, ~0.01 EUR/GB/mo).

Deliverable of Phase 1: a documented `provision.sh`/`teardown.sh` pair, a warm
500 GB volume, and recorded wall-clock + cost numbers in `validate/report.md`.

## Phase 2 - replicate clark's amd64 binary and validate parity

**Goal: our amd64 build is behaviorally identical to clark's published
`clark-browser-linux-x64.tar.gz`, proven, before touching arm64.**

1. **Copy clark's build inputs verbatim** into `packages/browser/`:
   `patches/` (all 26 + `000-shared/`), `build/Dockerfile.linux`,
   `build/build-linux.sh` (rename `CLARK_*` env to neutral names but keep logic
   identical), and pin `versions.env` to the exact same
   `UC_TAG=148.0.7778.96-1`. Do **not** "improve" anything yet - parity first.
2. Build amd64 on the warm volume (fast: incremental). Package the tarball.
3. **Validation harness `validate/parity.py`:**
   - Download clark's reference tarball (`CLARK_REF_*` from `versions.env`,
     sha256-checked) into a temp dir.
   - Launch **both** binaries headless over CDP with an **identical** flag set and
     `--fingerprint=<fixed seed>` (reuse the exact flags from
     `tests/linux_smoke.py` / cuttle's `ForkParityArgs` Windows persona).
   - Capture the full fingerprint surface from each: `navigator.*`
     (platform, userAgent, hardwareConcurrency, maxTouchPoints, webdriver,
     plugins, languages), UA-CH `getHighEntropyValues`
     (platform/platformVersion/architecture/bitness/fullVersionList), WebGL
     unmasked vendor/renderer, canvas/clientRects noise behavior across seeds,
     audio FP differential across seeds, `Intl` timezone/locale, `screen`
     coherence, `navigator.connection` datacenter profile.
   - **Assert every captured value is identical between our binary and clark's.**
     Diff -> fail with the offending vector. Also compare `--version` /
     `chrome://version` build string and the packaged file list.
4. Also run clark's `tests/linux_smoke.py` (as `validate/smoke.py`) against our
   binary - must exit 0.
5. Parity gate: `parity.py` must report **zero surface diffs** and smoke must
   pass. Record results in `validate/report.md`. Only then proceed.

Expected honest caveats to document (not blockers):
- Build string / version metadata may differ (LASTCHANGE stub, build date) - that
  is metadata, not fingerprint surface; note it explicitly as an allowed diff.
- If a fingerprint vector diffs, the cause is almost certainly a patch that
  didn't apply or a missing 000-shared wiring - debug against the `patch` logs.

## Phase 3 - build the linux/arm64 target (macOS persona)

**Only after Phase 2 parity is green.**

1. **Add a `target_cpu="arm64"` lane to `build-linux.sh`.** The script already
   installs the arm64 sysroot (`install-sysroot.py --arch=arm64`) in its
   arm64-*host* branch; here we need x64-host -> arm64-target, which Chromium's
   toolchain declares natively (unlike the reverse, which clark had to hand-add).
   Set in `args.gn`: `target_cpu = "arm64"`, `use_sysroot = true`; keep all other
   args identical to amd64 for parity of behavior.
2. Build arm64 on the warm volume. Discover and record any **arm64-only patch
   deltas** needed (line-number drift, arch guards). If a clark patch doesn't
   apply on the arm64 target, add a minimal `patches/arm64/NNNN-*.patch` overlay -
   do NOT edit the shared patches. Document each delta and why.
3. **Validate arm64 + macOS persona coherence** with an arm64/macos expectation
   set in `validate/smoke.py`:
   - UA-CH `architecture == "arm"`, `bitness == "64"` (the flipped assertion vs
     x86).
   - `navigator.platform` / UA = the frozen macOS values from
     `docs/2607-17-native-macos-backend.md`
     (`Intel Mac OS X 10_15_7`, `--fingerprint-platform=macos`).
   - WebGL unmasked renderer spoofed to an Apple/Metal string (no real GPU in the
     container -> SwiftShader software render underneath; the *string* is spoofed,
     the deep behavior reads as software - documented known delta, same class as
     the Windows-on-Linux setup today).
   - UA/CH single-source coherence: no `HeadlessChrome` token on the wire, one
     brand-version across the network and JS paths (clark's two-code-path leak,
     already root-caused in the macOS doc).
4. There is no clark arm64 reference binary to diff against (clark ships none), so
   arm64 validation is **internal coherence** (smoke expectations + no arch/UA/CH
   leaks), not cross-binary parity.

## Phase 4 - wire both binaries into cuttle's image and full-test

1. **Publish the two bundles as a GitHub release on `glim-sh/cuttle` (DECIDED,
   Q4).** From the build box, `gh release upload` both tarballs to a release tagged
   from `versions.env` (e.g. `browser-v148.0.7778.96-1`), pinned by sha256 -
   mirroring today's clark `ADD --checksum` pattern exactly, zero new infra.
   `packages/browser/versions.env` records our own release tag + assets + shas.
   NOT built inline in the Docker build (never couple the image build to a
   multi-hour Chromium compile).
2. **Dockerfile (`ops/docker/Dockerfile`)** -> multi-arch:
   - Replace the clark `ADD` with our own tarball per `TARGETARCH`
     (amd64 -> our windows-persona x64 bundle; arm64 -> our macos-persona arm64
     bundle).
   - Remove the `GOARCH=amd64` pin in the Go builder stage (use `TARGETARCH`).
   - Keep `/opt/clark/chrome` -> rename to a neutral `/opt/browser/chrome`; update
     `CUTTLE_BROWSER_BINARY`.
3. **macOS persona in Go (`internal/fingerprint/args.go`):** add a macOS branch to
   `ForkParityArgs` (and the `getDefaultStealthArgs` platform), selected by target
   arch / an env flag, with the exact flag set from the macOS doc. Point
   `--fingerprint-fonts-dir` at the **mounted macOS font dir** for the arm64 image
   (see fonts below) instead of `/opt/winfonts`.
4. **Fonts for the macOS persona (local-on-Mac):** bind-mount the host Mac's
   **pristine system font set** read-only into the arm64 container -
   `/System/Library/Fonts` (+ `/System/Library/Fonts/Supplemental`), **not**
   `~/Library/Fonts` (user-installed fonts add entropy and de-cohere the list).
   Point `--fingerprint-fonts-dir` there. This ships no fonts (no redistribution
   issue - the user's own OS fonts, local only) and yields a genuinely coherent
   Mac font list. Docker Desktop gotcha: `/System` isn't in the default VirtioFS
   shares - add it under Settings -> Resources -> File sharing, or copy the set to
   a shared dir and mount that. Document in `packages/browser/README.md`.
5. **Drop the emulation pins** so the arm64 image runs native on Apple Silicon:
   `Justfile` (`--platform` -> per-arch or `buildx` both), Helm arch selector (or
   parametrize), `internal/backend/local.go` (stop forcing `--platform
   linux/amd64`; select by host arch).
6. **Golden + smoke:** regenerate `internal/fingerprint/testdata/golden.json`
   (`just parity-golden`) to include the macOS `fork_parity_args` cases (they were
   removed with the old native backend; re-add per the doc). Run `test/smoke`
   (`go run ./test/smoke`) against **both** built images. The Windows/amd64 golden
   output must stay unchanged (regression tripwire).
7. **Full manual test on the MacBook:** run the arm64 image, confirm native (no
   Rosetta), novnc works, and the fingerprint surface is coherent-macOS on
   browserleaks/CreepJS-style probes.
8. **Widevine on amd64 (enhancement, after parity - see R5/Q6):** build the
   shipping amd64 bundle with the `enable_widevine=true` overlay, sideload Google's
   `linux-x64` CDM into the image, pre-seed the hint file, and run with a
   persistent `user-data-dir`. Validate EME playback AND that the CDM security
   level doesn't leak Linux under the Windows persona. arm64 Widevine is a
   deferred separate spike, not part of this phase.

## Validation strategy (summary)

> **SUPERSEDED 2026-08-19 (parity half only).** The clark parity gate is retired;
> see "First version bump" at the end of this doc, section C.

- **amd64: cross-binary behavioral parity** vs clark's published tarball
  (`parity.py`, zero surface diffs) + clark smoke pass. NOT byte-identical
  (impossible; documented).
- **arm64: internal coherence** (macOS-persona smoke expectations, no arch/UA/CH
  leaks) - no reference binary exists.
- **Integration: cuttle golden + `test/smoke`** on both images; Windows golden
  unchanged.

## Risks & open questions

- **R1 - patch ownership: FORK AND DIVERGE (DECIDED, Q1; VINDICATED 2026-08-19 -
  clark went dormant at 148 and skipped 149/150/151, so "fork and own" turned out
  to be the only viable option, not merely the preferred one).** We fork clark's series
  into `packages/browser/patches/` now and own it - not a downstream mirror. We
  pin to their current `stealth5` release only to validate Phase 2 parity once,
  then we're independent. There aren't many patches (26), so watching their repo
  for interesting changes and cherry-picking deliberately is easy; we do NOT
  continuously re-pull. Patch line-number drift on a Chromium version bump is an
  owned cost - document the re-diff procedure in the README.
- **R2 - the hand-stubbed runhooks are fragile** (`gclient_args.gni`, LASTCHANGE,
  DAWN/skia/gpu headers). They can break on any Chromium version change. Keeping
  clark's script verbatim in Phase 2 minimizes surprise; changes are Phase 3+.
- **R3 - arm64 target may need non-trivial patch deltas** we can't fully predict
  until Phase 1 step 3 / Phase 3. Surface early (that's why Phase 1 builds arm64
  too).
- **R4 - macOS WebGL is spoofed-string-over-software (ACCEPTED for now, Q5).** In
  the container there's no GPU; deep WebGL probes read as software, not Metal.
  Accepted, same class as Windows-on-Linux today. Documented as a **future angle
  to explore**: a SwiftShader->Metal-behavior coherence patch, only if a real
  target actually probes deep WebGL. Not built now.
- **R5 - Widevine/EME: WANTED (Q6). Achievable on amd64, a rabbit hole on arm64.**
  Researched; here is the real shape:
  - **We control the build, so we CAN enable it** - clark doesn't, but its
    ungoogled-148 base still compiles the EME host adapter. Add to a **separate
    shipping-args overlay** (NOT the Phase 2 parity build, which must stay
    clark-identical): `enable_widevine=true`, `enable_library_cdms=true` (on by
    default desktop), keep `proprietary_codecs=true` + `ffmpeg_branding="Chrome"`.
    None of these projects bundle the proprietary CDM (can't redistribute
    Google's blob); the working path everywhere is **sideload** - copy
    `WidevineCdm/` out of a real Google Chrome and drop it next to our binary.
  - **amd64: works, moderate effort.** Sideload Google's `linux-x64`
    `libwidevinecdm.so` at image-build time. CloakBrowser #96 documents the
    gotcha: with ephemeral profiles the CDM registers too late, so use a
    **persistent `user-data-dir` + pre-seed the
    `WidevineCdm/latest-component-updated-widevine-cdm` hint file**. This is a
    **Phase 4+ enhancement over clark**, added after amd64 parity is proven, and
    tested on its own (it diverges from clark's binary by design).
  - **arm64: OBSOLETE as of M149 (2026-06-02) - an official first-party linux-arm64
    CDM now exists. The two blockers below no longer apply; see the rewritten
    `2607-23-arm64-widevine-spike.md`. Original text kept for the record:**
  - **arm64: deep rabbit hole, DEFER to a research spike.** Two stacked blockers:
    (1) Google ships **no desktop linux-arm64 CDM** - the only aarch64 CDM lives
    inside ChromeOS LaCrOS images and needs extract + binary-patching
    (`GLIBC_ABI_DT_RELR`, aarch64 atomic stubs; the AsahiLinux `widevine-installer`
    / `pivine` do this); (2) we must ensure `widevine_cdm_component_installer.cc`
    actually compiles into the arm64 target (the exact thing CloakBrowser's arm64
    build missed, #349). Do NOT block arm64 delivery on this. Tracked as a
    standalone spike: `2607-23-arm64-widevine-spike.md`.
  - **Open coherence sub-question (both arches):** the CDM reports its own
    platform + security level (L1/L3). A **Linux CDM under a Windows or macOS
    persona** may itself be a tell (real Windows/Mac Chrome uses that OS's CDM
    with different characteristics). Verify the EME surface
    (`navigator.requestMediaKeySystemAccess` robustness/security level) doesn't
    leak the real OS before shipping Widevine on either persona. Enabling Widevine
    does help one thing: it clears the FingerprintJS `nodriver` flag that fires
    when storage quota is high with no CDM (CloakBrowser #320/#96).
- **R6 - Hetzner pricing volatility.** Prices moved repeatedly in 2026; confirm
  live at Phase 1. The hcloud context/token is deliberately deferred to Phase 1
  start (Q7), not a standing blocker.

## Cost (ephemeral, not monthly rental)

- Build box: CCX53/CCX63 spun up per build, destroyed after -> **single-digit EUR
  per full build run**; incremental rebuilds on the warm volume are minutes.
- Standing cost: the 500 GB volume (~a few EUR/mo) holding the warm cache.
- Not a monthly dedicated rental - a 4-6h build does not justify one.

## Execution order (checklist for the fresh session)

1. Phase 0: hcloud context + ssh key + size decision.
2. Phase 1: `provision.sh` -> warm volume (both targets built once) -> `teardown.sh`.
3. Phase 2: copy clark inputs -> amd64 build -> `parity.py` green -> record report.
4. Phase 3: arm64 lane -> build -> macOS-persona coherence green -> record deltas.
5. Phase 4: Dockerfile multi-arch + macOS persona Go + font mount + drop emulation
   pins + golden/smoke on both images + manual MacBook test.

---

# First version bump: 148 -> 151 (2026-08-19) - decision log

The first real exercise of this pipeline's version-bump path. It invalidated
several assumptions above and surfaced five latent bugs that would have broken
*any* bump, not just this one. Recorded here so none of it is rediscovered.

## A. Why bump at all

The competitive landscape moved and we did not:

| Project | Chromium | Status |
|---|---|---|
| clark-browser | **148.0.7778.96** | **Dormant.** Last release 2026-06-01, last commit 2026-06-22. Skipped 149, 150, 151. |
| [CloakBrowser](https://github.com/CloakHQ/CloakBrowser) | 151 | Active, but binaries went license-keyed; free build frozen at 146 |
| [ChromiumFish](https://github.com/arman-bd/chromiumfish) | 151 | Active, fully MIT, forks vanilla Chromium (not ungoogled) |
| [clearcote-browser](https://github.com/clearcotelabs/clearcote-browser) | 151 Pro / 149 free | Active, ungoogled base, tiered releases |

Two pieces of direct evidence that a stale major degrades in practice, both from
the CloakBrowser maintainer:

- On their frozen 146 free build ([#503](https://github.com/CloakHQ/CloakBrowser/issues/503)):
  *"Anti-bot systems track browser versions, and it's simply outdated at this
  point. It's not expected to pass current Cloudflare Turnstile."*
- On spoofing an older major via headers ([#499](https://github.com/CloakHQ/CloakBrowser/issues/499)):
  *"Changing the User-Agent and Client Hints cannot make Chromium 150 behave like
  Chromium 148; that approach stopped being reliable many years ago."*

That second one matters strategically: there is no cheap escape. Staying current
requires actually building the current major.

**Cadence conclusion.** ChromiumFish's two consecutive resyncs cost roughly the
same (~22 patch files, ~350 lines each) whether the hop was 149->150 or 150->151.
Per-major cost is close to constant and does not compound *if you never skip*.
Sitting out three majors is what made this expensive. Bump every major.

## B. Which version, and the ungoogled coupling

Target `151.0.7922.137` / `UC_TAG=151.0.7922.137-1`, not the newest Chrome patch
build. We are gated on ungoogled having tagged a release, and its newest tag was
`.137` while Chrome stable was `.169`. Being one patch build behind at the right
major is fine; being three majors behind is not.

## C. Retiring the clark parity gate (the biggest structural change)

`parity.py` asserted zero fingerprint-surface diff against clark's stealth5
tarball. Retired, for three reasons:

1. **Its job was finished and already recorded.** It existed to prove the
   self-hosted pipeline reproduced clark's binary during the fork transition. A
   gate that can only ever pass once has stopped being a test.
2. **It had inverted into a brake.** Every future stealth improvement must break
   byte-parity with a frozen 148 binary by design. The plan above already
   conceded this for the `0027`/`0047` cherry-picks.
3. **No counterpart can exist at 151.** clark is dormant; there will never be a
   151 reference.

**What replaced it, and what was deliberately kept.** `validate/smoke.py` is now
the pass/fail gate. The `parity.py` *machinery* (launch two binaries with an
identical flag set and fixed seed, capture the full surface, byte-diff) is the
most valuable test asset in the repo and was kept - repointed at our own previous
release. A bump now answers "which fingerprint vectors moved between our N-1 and
our N", which is exactly the review artifact a rebase needs.

**Consequence to remember:** retiring the oracle removes a safety net that used
to catch silent stealth drift. That is precisely why the `-F0` change in D became
necessary rather than merely nice.

## D. Patch mechanics

### D1. `-F0` for our series, `-F3` for ungoogled's

`build-linux.sh` applied everything with `-F3` (fuzz 3). On this rebase that let
`0013-window-outer-and-dpr` **apply with a green exit while landing its hunks in
the wrong functions**: `outerHeight`'s body went inside `LocalDOMWindow::confirm()`,
and a `return static_cast<int>(...)` went inside `prompt()`, splitting a string
literal. Cause: 151 rewrote `if (!GetFrame()) return 0;` into the braced form,
which killed the 3-line leading context of two pure-insertion hunks; with `-F3`
all context is discarded and `patch` falls back to raw line-number placement.

The build would have failed to compile, but the quieter half is worse:
`outerWidth`/`outerHeight` would have been left **completely unpatched** while the
run reported success. With the parity oracle gone, nothing else would have caught
it.

Our series is now `-F0`. Ungoogled's stays `-F3` because three of its patches
genuinely need fuzz and are upstream-authored against the exact tag. Note `-F0`
does **not** break offset-only patches: `patch` treats offset (same context found
at a different line) and fuzz (context discarded) as separate mechanisms.

### D2. Never hand-write hunk headers

`patch` rejects on *counts*, not content. Three separate attempts at hand-computed
`@@ -a,b +c,d @@` headers failed - twice from miscounting context lines, once from
a phantom trailing blank line that made the header declare 7 context lines where 6
existed. The context bytes were verified identical to the source each time; the
arithmetic was the defect.

**The reliable method**, used for the final `0013`, `0048` and `0049`: copy the
target file, apply the intended edit programmatically, then `diff -u orig new` and
paste the generated hunks under our header comment. `diff` cannot miscount.

### D3. Re-applying after a failure requires a full tree reset

A partially applied patch makes `--forward` report "previously applied" and skip
*every* hunk, so the second attempt is not a retry - it is a no-op that looks like
a different failure. Stage 3's reset must be all three of:

- `git reset --hard HEAD` (~2s; reverts the ~660 files ungoogled modifies)
- `git submodule foreach` reset - **three ungoogled patches edit files inside the
  `v8` and `third_party/devtools-frontend` submodules**, which a top-level reset
  cannot reach. 266 submodules, ~5s.
- `git clean -fd -e uc_staging` - removes files ungoogled *adds*. Never `-x`,
  which would delete ~19 GB of gclient-managed `third_party`; `-e uc_staging`
  preserves depot_tools, matching what `clone.py` itself preserves.

### D4. What the 25 patches actually needed

> Recorded during the rebase, when the series was 25. `0041` was deleted
> afterwards (see L2), so the series is 24 today. The counts below are the
> state at rebase time and are left as measured.

12 clean, 6 offset-only, 5 fuzz, 1 hard failure. No patched file was removed or
renamed. Notable:

- `0048` was the only genuine rewrite: `PermissionStatusListener::status_` changed
  from a plain enum to a mojo StructPtr (`PermissionStatusWithDetailsPtr`). The
  helper now **mutates in place** rather than taking and returning by value - a
  StructPtr is move-only, so round-tripping it through a return type fights the
  ownership model. Mutating the incoming status before the `Equals()` compare also
  means observers receive the already-normalised `status_.Clone()` with no further
  change.
- `0006` and `0050` each had hunks that were **redundant because ungoogled 151 now
  does what we did**: the `kReducedSystemInfo` check in `hardwareConcurrency()`,
  an ungoogled switches include, and three `kFingerprinting*Noise` entries. The
  fork is converging with upstream, not diverging.
- `0006`'s redundant lines had to become **context**, not vanish: our override
  must run *before* ungoogled's `kReducedSystemInfo` short-circuit or the
  fingerprint value is clobbered to 2.
- `0013`'s override was deliberately placed **after** 151's new
  `frame->IsInFencedFrameTree()` guard, so fenced frames keep Chrome's
  `innerWidth`/`innerHeight` semantics. Returning screen metrics there would leak
  what Chrome deliberately withholds - a new tell created by fixing an old one.
- `0027-analyser-node-noise` was cherry-picked (retiring the parity gate removed
  the reason not to). `realtime_analyser.cc` changed by only 2 lines 148->151.
- `0047-suppress-cdc-globals` was evaluated and **deliberately not taken**: it
  registers its V8 extension in `headless/lib/renderer/headless_content_renderer_client.cc`
  and we run headed, so it never executes. The `cdc_` globals it strips are
  chromedriver artifacts and we drive raw CDP. `0002` and `0045` live in the same
  headless embedder files and are likely dead for the same reason.

## E. Five latent pipeline bugs

All pre-existing, all would break any bump:

1. **Tag checkout aborted on a dirty tree.** `build-linux.sh` edits
   `ungoogled-chromium/utils/clone.py` in place (the gsutil defang), so
   `git checkout <new tag>` refused with "local changes would be overwritten".
   Fixed with `checkout -f`.
2. **The tree reset never worked.** `git checkout -- .` silently failed to revert
   ungoogled's ~660 modified files (its errors were swallowed by `2>/dev/null ||
   true`), plus the submodule gap in D3. The tree could therefore never be
   re-prepared.
3. **Empty patch dir tripped `set -e`.** An unmatched glob passed the literal
   string to `sha256sum`. Fixed with `shopt -s nullglob`, which is what makes a
   patch-free cache-warming build possible at all.
4. **Dawn needs a Go toolchain in 151.** `third_party/dawn/tools/generate-sources-gn.py`
   shells out to Go for Tint source generation. Its DEPS entry is gated on
   `non_git_source`, which our recovery `.gclient` sets false, so gclient never
   fetches it and the build dies ~3,200 targets in with `FileNotFoundError`. The
   generator runs on the **host**, so only `linux-amd64` is needed regardless of
   `TARGET_CPU`. Now installed via cipd with the version read from the tree's own
   DEPS so it cannot drift.
5. **`args.gn` ignored ungoogled's `flags.gn`.** See F - the most consequential.

## F. `args.gn` must merge ungoogled's `flags.gn`

`build-linux.sh` wrote `args.gn` from scratch and never sourced ungoogled's
`flags.gn`. That is not a configuration preference - **ungoogled's patch series is
authored against those flags, so omitting them miscompiles.**

The concrete failure: `fix-building-without-mdns-and-service-discovery.patch`
strips `service_discovery_client_` from `dns_sd_registry.h`, but we left
`enable_service_discovery` at its default `true`, so the `.cc` still referenced a
member that no longer existed. The build died ~30,000 targets and 52 minutes in.

Twelve flags were missing: `enable_mdns`, `enable_service_discovery`,
`enable_reporting`, `enable_hangout_services_extension`,
`disable_fieldtrial_testing_config`, `google_api_key`,
`google_default_client_id`, `google_default_client_secret`,
`use_official_google_api_keys`, `use_unofficial_version_number`,
`exclude_unwind_tables`, `v8_drumbrake_bounds_checks`.

**The Google-API-key ones matter beyond the build break.** Every binary this
pipeline produced before 151 was compiled with default Google API keys and
field-trial testing config enabled, in a browser whose entire purpose is not
phoning home.

Now merged flags-first with our overrides layered on top, six overlapping keys
filtered so there is exactly one assignment each, and a hard failure if
`flags.gn` is missing. Side effect: the graph shrank from 53,658 to 32,844
targets (x64) and 90,494 to 61,275 (arm64).

**A concern raised and then retracted.** `enable_reporting=false` and
`enable_mdns=false` look like they could remove web-observable behaviour real
Chrome has (`ReportingObserver`; mDNS `.local` ICE candidates). Two things argue
otherwise: clearcote builds on the same ungoogled base and ships the **identical**
flag set despite a design document obsessed with coherence; and clearcote
simultaneously lists "mDNS `.local` host candidate" as an implemented P0 surface,
which is only possible if the gn flag and the WebRTC behaviour are different
subsystems (they are - `enable_mdns` guards `chrome/browser/local_discovery`,
i.e. Cast and printer discovery). Still worth **measuring** `ReportingObserver`
presence on the 151 binary rather than assuming.

## G. Widevine

**Decision: enable the gn flag now, defer the runtime fetcher.** The two halves
separate cleanly - `enable_widevine = true` is a build-time decision that would
otherwise cost a full rebuild to add later; the CDM fetcher is runtime and can
land independently.

**Licensing position (both arches).** `bundle_widevine_cdm` stays false because we
are not chrome-branded, so we compile key-system support and ship **no proprietary
blob** in the binary, tarballs or image. The operator's container fetches the CDM
at runtime from Google's endpoint. Upstream explicitly sanctions enabling it on
non-Android platforms and ungoogled's own `flags.gn` sets it. clearcote reached
the same position independently.

**arm64 is now viable** - see the rewritten `2607-23-arm64-widevine-spike.md`.
R5's arm64 analysis above was correct when written and was obsoleted by M149.

**Constraint that shapes the fetcher design:** the CDM registers before the zygote
locks down, so it cannot be hot-swapped or lazily fetched into a running browser.
The fetch must complete before the daemon launches Chrome for a seed.

**Measured value, honestly:** clearcote's own audit moved 97/100 -> 98/100 and
identity 58/61 -> 59/61 when seeding the CDM. Real, modest.

## H. Build-host and cache decisions

- **One builder, not two.** Parallel boxes halve wall clock but save ~2h once per
  major, against a second box to provision and tear down and a manual quota
  increase. Not worth it monthly. Better middle path: run both targets
  **concurrently on one box** once the tree is prepared - stages 1-3 are no-ops
  for the second target, and the July build measured throughput collapsing from
  ~27 to ~5 targets/sec in the link-heavy tail, so two overlapping builds fill
  each other's idle cores.
- **Build on the host's local NVMe, not the network volume.** The volume existed
  to persist a tree assumed expensive to rebuild. Measured: full prep (clone,
  gclient sync, ungoogled apply) is **6.5 minutes**. Local NVMe is also faster for
  a tree of millions of small files, where the volume is IOPS-bound.
- **What is actually worth persisting is `out/`, not the source.** This is the
  subtle one. sccache makes a *cold* build cheaper but still invokes the compiler
  for every target and **does not cache link steps** - which are the serialized
  slow tail. A preserved `out/` tree gives true ninja incrementality: change one
  file, relink, done in minutes. `out/` is invalidated by **mtime**, so any
  tar/object-storage round trip silently dirties it (1-second granularity vs
  ninja's nanoseconds) while appearing warm.
- **Cache-warm builds are worth running while patches are still being fixed**, but
  only because the box would otherwise be idle. A *patched* build warms sccache
  identically, so warming is not a reason to build twice in the normal case.

**Measured timings (48 vCPU dedicated):** full source prep 6m38s. Cold 148 builds
were ~2h08m (arm64) and ~2h-2h30m (x64) at ~82,600 targets. Post-`flags.gn` 151
graphs are ~40% smaller.

## I. Measured stealth findings, and two corrections

An empirical probe of the shipping 148 binary produced findings that should be
handled as a **separate change** from this rebase. Highest-value first: shader
dialect contradicts the renderer string (SPIR-V/ANGLE-Vulkan banner beside a
claimed D3D11 or Metal renderer); CSS `@media (device-width)` returns the real
framebuffer while `screen.*` returns the persona; `(pointer:none)`/`(hover:none)`
report no input device at all; WebGL capability limits and extension list
contradict the spoofed GPU quantitatively; `toBlob`/`convertToBlob`/`getBBox`
bypass canvas noise and return a value **identical across every seed**;
`speechSynthesis.getVoices()` returns 0; `enumerateDevices()` returns `[]`;
WebRTC yields zero ICE candidates.

Every bot *classifier* passes (sannysoft, BrowserScan, CreepJS headless checks).
Every fingerprint *consistency* checker flags the same four surfaces: Screen,
Canvas, Audio, WebGL.

**Two corrections that must not be regressed:**

1. **`0036-storage-estimate-from-cli` is dead code, not a live tell.** Reading the
   patch suggests it reports a constant 9.96% usage ratio. Measured, live
   behaviour matches stock Chrome byte-for-byte (quota grows as `10 GiB + usage`).
   The constant only fires with `--fingerprint-storage-quota`, which the daemon
   never passes. **Enabling it would create the tell it was meant to prevent.**
2. **The Windows-UCRT `Math.tanh` port is unresolved, not recommended.** Our two
   builds produce byte-identical transcendentals, and V8 ships its own in-tree
   fdlibm port, which suggests no OS signal exists. That collides with
   ChromiumFish's claim of validating their port bit-identical against real
   Windows Chrome. The decisive test is against a real Windows Chrome, which has
   not been run. **Do no work here until it is.**

Also relevant to the roadmap: CloakBrowser's maintainer states plainly that
*"hiding the WebRTC IP entirely is itself a detection signal"*
([#95](https://github.com/CloakHQ/CloakBrowser/issues/95)), and that the correct
shape is an obfuscated `.local` hostname for the host candidate with the public
candidate tracking the proxy exit
([#298](https://github.com/CloakHQ/CloakBrowser/issues/298)). Our zero-candidate
behaviour is the state they call out as a tell.

## J. Options considered and rejected

- **Consume ChromiumFish binaries instead of building.** Faster, but re-creates
  the exact single-vendor dependency that just died with clark. Its patch series
  is also not drop-in: it forks vanilla Chromium, not ungoogled, and carries UI /
  ai-agent / canvas-bridge patches we do not want. Kept as a *reference* for 151
  conflict resolutions, which is genuinely valuable.
- **Per-eTLD+1 canvas farbling** (clearcote's headline feature, and their critique
  of the single-seed approach as a cross-site linking supercookie). Correct for a
  privacy browser, **wrong for us**: real Chrome hands every site the same canvas
  hash because it is the same GPU. A browser whose canvas changes per site is
  anomalous in a way real hardware never is. Our one-seed-per-identity model is
  already right. Recorded so nobody "fixes" it later.
- **`X-Browser-Validation` request-integrity headers.** Scoped to Google-associated
  domains only (`google_util::IsGoogleAssociatedDomainUrl`: Google search TLDs,
  YouTube, plus `.gstatic.com`, `.googleapis.com`, `.doubleclick.net` and similar).
  The per-milestone seed rotates *within* a major as well as between, and each
  rotation must be recovered byte-exact from a real Chrome binary - unscheduled
  manual reverse-engineering as a permanent tax. A **stale** seed is worse than an
  absent header. Skip unless Google properties become a target.
- **Server snapshots instead of the cache volume** (evaluated in the original
  plan, revisited here). Hetzner snapshots capture the root disk only, so the
  build must live there rather than on an attached volume; snapshots bill on used
  size, not disk size. Viable, but restores are pinned to a machine type at least
  as large as the source disk.
- **A bigger build box.** None exists - we already use the largest dedicated type
  offered. The link-heavy tail underutilizes the cores we have anyway.

## K. Open questions carried forward

1. Does `ReportingObserver` survive `enable_reporting=false`? Measure on 151.
2. Do ICE candidates appear at all, and can we get the obfuscated `.local` host
   candidate CloakBrowser describes?
3. Is the Windows-UCRT `Math.tanh` delta real? Needs a real Windows Chrome.
4. ~~Does a Linux arm64 Widevine CDM under a macOS persona leak non-macOS through
   the EME surface?~~ **Largely answered 2026-08-19: real macOS Chrome is L3.** It
   exposes no `HW_SECURE_*` and does not route EME through FairPlay, so a Linux L3
   CDM should present the same ladder. Verified against a real, everyday profile
   with no CDP (61/61 values identical to the automated run). Remaining work is one
   throwaway fetch + diff against the arm64 build, NOT the productionised fetcher.
   **New question in its place: the WINDOWS persona is now the riskier one** -
   Windows Chrome on hardware DRM advertises `HW_SECURE_*`, which our Linux CDM
   cannot. Unmeasured; we have no real Windows Chrome reference. Do not assume it
   is safe by analogy with the macOS result. See the arm64 spike doc.
5. Are `0002` and `0045` dead like `0047`? Probing could not distinguish them from
   the natural headed path; needs a source read or a `--headless` run.

## L. What compiling the series found that applying it could not

The `-F0` apply gate reported 25/25 clean. Three defects survived it, and every
one was caught by the compiler or by a compiler warning.

### L1. `-F0` is meaningless for a context-free hunk

A hunk written `@@ -192,0 +197,94 @@` has no context lines, so `patch` inserts it
at the recorded line number and cannot fail at any fuzz level. The gate's green
result carried no information for exactly the patches that most needed checking.
On 151 this dropped `0016`'s GPU-pool helpers inside the body of the multi-line
`POPULATE_TEX_SUB_IMAGE_2D_PARAMS` macro.

Only three patches had context-free hunks: `0016` (8 of 11, broken), `0010`
(6 of 6, landed correctly by luck) and `0011` (1 of 8, an include). Audit with
`grep -cE '^@@ -[0-9]+,0 \+[0-9]+' patches/0*.patch` before trusting any clean
apply. All three were verified by reading the patched tree; `0016` was
regenerated with real context.

### L2. `0041` deleted - it was fighting the daemon and no longer compiled

Two flips, neither of which survived scrutiny:

- `kNoReferrers` was already countermanded on every launch by the
  `--disable-features` value in `ForkParityArgs`. Dead weight.
- `kClearDataOnExit` is not an upstream feature; ungoogled adds it. Enabled, it
  calls `BrowsingDataRemover` with an all-types mask on **every browser exit** -
  cookies, site data, history, passwords, form data - directly defeating the
  per-seed profile persistence the daemon is built around. This had been shipping
  since 148.

It also stopped compiling: because ungoogled *adds* the definition rather than
Chromium shipping it, our hunk duplicated it instead of replacing it. `0040`
survives and still enables the other two referrer features, so the
`--disable-features` value stays.

### L3. Regenerating a patch can silently drop a hunk

Rebuilding `0016` from a reverted base omitted the `UNMASKED_VENDOR/RENDERER`
rewrite - the core of the patch. With `0019` enabling `kSpoofWebGLInfo`, the
build would have returned ungoogled's default parameter, a single space, as the
WebGL renderer string. Nothing failed; the only signal was `-Wunused-function` on
`ClarkNonBlankFeatureParam`, whose two call sites were both inside the missing
hunk.

After regenerating any patch, diff the set of added identifiers against the
original and read the build log for unused-symbol warnings.

### L4. The smoke gate cannot run if its harness is not mounted

`build-linux.sh` looks for `$WORK/packages/browser/validate/smoke.py` and exits
`2` when it is absent rather than skipping. `run-build.sh` mounts it; an ad-hoc
driver that does not will compile and then refuse to package. That is the correct
failure: the smoke gate runs *before* the tar, so a skipped gate would otherwise
publish an unvalidated binary.


## M. Version coherence (2026-08-19)

### M1. One version constant, two tripwires

The pinned Chromium version was hardcoded at eight sites across four languages.
`packages/browser/versions.env` is now the single source: the validate harness
reads `CHROMIUM_VERSION` from it directly, and the Go side keeps one literal
(`internal/fingerprint/args.go` `chromiumVersion`) because `go:embed` cannot
reach outside its package directory. Two tests close the gap - `TestChromiumVersionPin`
(Go literal vs `versions.env`) and `TestPersonaVersionsMatchSmoke` (the persona
OS versions the daemon emits vs the ones the smoke gate asserts). Both were
verified to actually fail when their inputs are perturbed; a tripwire nobody has
seen fail is not known to work (see L1).

Everything else derives: the reduced `X.0.0.0` form the UA carries is computed
from the pinned build, never written down separately.

### M2. Why the version sites had to converge

Measured on the 151 binary: with a `--fingerprint` persona active the browser
stamps its own real version into `navigator.userAgent` regardless of what
`--user-agent` says, while the HTTP `User-Agent` header follows the switch and
UA-CH follows `--fingerprint-brand-version`. Three surfaces, three sources. With
the pre-bump values on a 151 binary they read 148 / 151 / 148 - a header-vs-JS
disagreement that is trivially detectable server-side. Coherence here is a
correctness property, not tidiness.

(Without any `--fingerprint-*` switches the `--user-agent` value is honoured
verbatim, including a nonsense `Chrome/777.0.0.0`; the rewrite is persona-path
only.)

### M3. The macOS persona has to match the machine its voice table came from

The persona advertised `platformVersion 15.0.0`. Real Chrome on the Mac the
macOS voice table was captured from reports `26.7.0`, and that table (199 voices,
180 local) is stock for that OS - reconciled against `say -v '?'` (184 installed,
Chrome exposes 180; the four it drops are Aman, Aru, Ona, Tara) and confirmed to
carry no Enhanced/Premium downloads, which would have renamed the base voices.
Published counts of 176/157 are macOS 14.5, two majors older. A persona claiming
an OS its voice list never shipped with is incoherent either way, so the two are
now pinned together at 26.7.0.

### M4. parity.py: from clark oracle to self-baseline

`versions.env` dropped the `CLARK_REF_*` keys when the oracle was retired, which
left `parity.py` unable to resolve a reference at all - it exited 2 unless an
explicit path was passed. It now diffs against our own last published release
(`BROWSER_RELEASE_TAG` + the per-arch asset and sha256, downloaded and verified
before execution). Exactly one vector may legitimately differ across a version
bump - `navigator.userAgent`, per M2 - and it is tolerated only when the two
strings match after masking the `Chrome/<version>` token, so a userAgent diff of
any other shape still fails.

First run against the previous release: one diff, the expected one. Canvas,
WebGL vendor/renderer, rects, plugins, screen, connection, timezone, locale and
the whole UA-CH block were byte-identical.

### M5. navigator.userAgentData built its own GREASE

Patch `0007` has two halves. The browser half derives the GREASE brand and the
brand ordering from the major version via upstream's
`GetGreasedUserAgentBrandVersion` + `ShuffleBrandList`, feeding `Sec-CH-UA`. The
Blink half - which is what `navigator.userAgentData` actually reads - hardcoded
`{"Not A(Brand", "24"}` and never shuffled, so JS contradicted the header our own
browser process sends, and did so identically for every browser version.

Upstream's algorithm is 11 greasy characters, 3 greased versions and 6 orderings,
all indexed by the major version. Reimplemented in the Blink half (Blink cannot
link the embedder) and checked against three independent measurements before
compiling: real Chrome 151 on macOS gives `Not=A?Brand`/99 ordered
[GREASE, Google Chrome, Chromium], our own stock 148 binary gives `Not/A)Brand`,
Chrome for Testing 152 gives `Not?A_Brand`. The port reproduces all three.

### M6. A cross-platform comparison is not a control

Our container sent no `Sec-CH-UA*` request headers while real Chrome on macOS
sent them on every request, which looked like a serious defect. It was not:
upstream Chrome for Testing 152, run in the same container through the same
harness, sends none either. The finding was an artifact of comparing a Linux
container against a macOS desktop. Same shape as the `about:blank` secure-context
trap - when a surface looks broken, reproduce it on an unmodified binary in the
same environment before believing it.

### M7. The arm64 artifact can be gated, just not on the build host

`build-linux.sh` prints that arm64 "cannot be smoked on this amd64 host" and
packages it ungated. That is true of the build host and was read as true in
general, so the arm64 binary shipped on the strength of the x64 gate plus a
later image build. It can be gated directly, natively, on an Apple Silicon
machine:

    docker run --platform linux/arm64 \
      -v <extracted-build>:/opt/browser-new:ro -v <websocket-client>:/pylibs:ro \
      -v packages/browser/validate:/work/packages/browser/validate:ro \
      -v packages/browser/versions.env:/work/packages/browser/versions.env:ro \
      -e PYTHONPATH=/pylibs -e BROWSER_BINARY_PATH=/opt/browser-new/chrome \
      -e BROWSER_FONTS_DIR=/opt/personafonts \
      --entrypoint bash ghcr.io/glim-sh/cuttle:latest -c \
      'Xvfb :99 -screen 0 1440x900x24 & export DISPLAY=:99; python3 .../smoke.py'

The published runtime image supplies the macOS font pack and a python3; it has
no pip and no `websocket` module, so the pure-python wheel is unpacked and
mounted instead. **Xvfb is not optional**: without a display the WebGL vector
comes back as two empty strings, which reads exactly like a broken GPU spoof.
The image's own previously-released binary reproduces that empty result, which
is what identified it as a harness artifact rather than a regression - the same
control that retracted M6. With the display up, all 20 assertions pass.

This gate is strictly stronger than the x64 one, which runs on the build host
with no font pack and skips the font vectors entirely.
