# Self-hosted stealth-Chromium build pipeline

Status: **plan / not started.** Branch: `feat/chromium-build-pipeline`. Author
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

- **Hetzner API token + hcloud context.** `hcloud` is installed (v1.66.0) but has
  **no active context** (`hcloud context list` shows none). Create a project API
  token in the Hetzner Cloud console, then `hcloud context create cuttle-build`
  and paste it. Every Phase 1 command assumes this context is active.
- **SSH key in the project:** `hcloud ssh-key create --name cuttle-build --public-key-from-file ~/.ssh/id_ed25519.pub` (or reuse an existing one). Needed so `provision.sh` can attach it.
- **Decide the build box size.** Cross-compiling both targets from one amd64 box.
  Chromium wants 32 GB+ RAM (64 GB comfortable at link), 200 GB+ NVMe, many cores.
  Recommended: **CCX53** (32 dedicated vCPU / 128 GB) for the sweet spot, or
  **CCX63** (48 vCPU / 192 GB) for fastest full builds. (CCX = dedicated-vCPU
  AMD EPYC, hourly-billed, destroy after use.) Confirm live pricing at checkout;
  Hetzner moved prices repeatedly in 2026.
- **Volume size:** shared src ~80 GB + `out/amd64` ~30 GB + `out/arm64` ~30 GB +
  toolchains + headroom. **Provision 300 GB** (~a few EUR/mo standing cost;
  volumes persist independently of the server).

## Phase 1 - Hetzner build box + volume + warm cache

**Goal: one-time materialization of the persistent build state, then stop paying
for compute.** After this phase the 300 GB Volume holds a fully-synced Chromium
checkout, all fetched toolchains, and (ideally) one completed build per target so
later phases get incremental (minutes) rebuilds instead of the ~1h clone + hours
of first compile.

Steps:

1. `packages/browser/hetzner/provision.sh`:
   - `hcloud volume create --name cuttle-build-cache --size 300 --location <loc>`
   - `hcloud server create --name cuttle-builder --type ccx53 --image ubuntu-24.04
     --ssh-key cuttle-build --volume cuttle-build-cache
     --user-data-from-file cloud-init.yaml`
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
4. **Optional enhancement:** enable `cc_wrapper="sccache"` in `args.gn` with
   `SCCACHE_DIR=/work/sccache` so the cache survives even a `git clean` of the
   src tree and speeds cross-target rebuilds. Skip if the volume-based incremental
   is enough (KISS).
5. **Stop compute, keep cache:** `packages/browser/hetzner/teardown.sh` runs
   `hcloud server delete cuttle-builder` and **leaves the volume**. (Powering off
   still bills on Hetzner; deleting the server is the real "stop." The warm state
   is on the volume, not the server root disk - that is why the checkout MUST live
   under the mounted `/work`.) Optionally `hcloud image create` a snapshot of the
   configured server to skip re-installing docker next time (cheap, ~€0.01/GB/mo).

Deliverable of Phase 1: a documented `provision.sh`/`teardown.sh` pair, a warm
300 GB volume, and recorded wall-clock + cost numbers in `validate/report.md`.

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

1. **Publish/stage the two bundles.** Simplest: on the build box, upload both
   tarballs to a place the Docker build can fetch (a GitHub release on our repo,
   or an object store), pinned by sha256 - mirroring today's `ADD --checksum`
   pattern. `packages/browser/versions.env` holds our own tags/shas.
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

## Validation strategy (summary)

- **amd64: cross-binary behavioral parity** vs clark's published tarball
  (`parity.py`, zero surface diffs) + clark smoke pass. NOT byte-identical
  (impossible; documented).
- **arm64: internal coherence** (macOS-persona smoke expectations, no arch/UA/CH
  leaks) - no reference binary exists.
- **Integration: cuttle golden + `test/smoke`** on both images; Windows golden
  unchanged.

## Risks & open questions

- **R1 - patch line-number drift on version bumps.** clark's patches are pinned to
  `148.0.7778.96-1`; bumping Chromium means re-diffing. Owned cost; document the
  bump procedure in the README. (Q: do we track clark's future patch updates, or
  fork the series now and diverge?)
- **R2 - the hand-stubbed runhooks are fragile** (`gclient_args.gni`, LASTCHANGE,
  DAWN/skia/gpu headers). They can break on any Chromium version change. Keeping
  clark's script verbatim in Phase 2 minimizes surprise; changes are Phase 3+.
- **R3 - arm64 target may need non-trivial patch deltas** we can't fully predict
  until Phase 1 step 3 / Phase 3. Surface early (that's why Phase 1 builds arm64
  too).
- **R4 - macOS WebGL is spoofed-string-over-software** in the container; deep WebGL
  probes read as software, not Metal. Accepted, same class as Windows-on-Linux
  today. (Q: is that acceptable for the intended targets, or do we need a
  SwiftShader->Metal-string coherence patch?)
- **R5 - Widevine DRM isn't compiled into arm64 Chromium builds** (CloakBrowser
  #349). Irrelevant unless a target gates on EME.
- **R6 - Hetzner pricing volatility / no active hcloud context yet.** Phase 0
  blocker; confirm live pricing.
- **Q7 - publish location for our tarballs** (own GitHub release vs object store)?
  Affects Phase 4 step 1.

## Cost (ephemeral, not monthly rental)

- Build box: CCX53/CCX63 spun up per build, destroyed after -> **single-digit EUR
  per full build run**; incremental rebuilds on the warm volume are minutes.
- Standing cost: the 300 GB volume (~a few EUR/mo) holding the warm cache.
- Not a monthly dedicated rental - a 4-6h build does not justify one.

## Execution order (checklist for the fresh session)

1. Phase 0: hcloud context + ssh key + size decision.
2. Phase 1: `provision.sh` -> warm volume (both targets built once) -> `teardown.sh`.
3. Phase 2: copy clark inputs -> amd64 build -> `parity.py` green -> record report.
4. Phase 3: arm64 lane -> build -> macOS-persona coherence green -> record deltas.
5. Phase 4: Dockerfile multi-arch + macOS persona Go + font mount + drop emulation
   pins + golden/smoke on both images + manual MacBook test.
