# packages/browser - self-hosted stealth-Chromium build pipeline

Builds cuttle's stealth-Chromium binary ourselves instead of consuming clark's
prebuilt release. A patch series over ungoogled-chromium, built with stock
depot_tools/gn/ninja on an ephemeral Hetzner box, for two targets:

- **linux/x64** - Windows persona. What cuttle ships/runs on remote hosts. Must
  reach validated behavioral parity with clark's published amd64 tarball.
- **linux/arm64** - macOS persona. So the image runs native (no Rosetta/QEMU)
  in Docker on Apple Silicon, with a coherent `architecture: arm` UA-CH hint.

Full rationale and phase plan: `docs/plans/2607-23-self-hosted-chromium-build-pipeline.md`.

## Layout

```
patches/          forked VERBATIM from clark @ chromium-v148.0.7778.96-stealth5
  000-shared/     clark_fingerprint_switches.{h,cc}, clark_seed.{h,cc}, BUILD.gn.fragment
  00NN-*.patch    24 patches (the exact set clark shipped in stealth5)
build/
  Dockerfile.linux  ubuntu:24.04 build image + pinned sccache
  build-linux.sh    parametrized by TARGET_CPU=x64|arm64; sccache-cached
  run-build.sh      docker driver on the Hetzner host (persistent /work volume)
hetzner/
  cloud-init.yaml   installs docker, mounts the cache volume at /work
  provision.sh      hcloud: create volume + server (idempotent, warm-cache safe)
  teardown.sh       hcloud: delete server, KEEP volume
validate/
  smoke.py          per-persona behavioral smoke (windows|linux|macos)
  parity.py         cross-binary surface diff: ours vs clark's tarball
  report.md         (generated) parity/coherence results
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

The `/work` volume holds the ~80 GB Chromium checkout, fetched toolchains,
`out/{x64,arm64}`, and the sccache cache. It persists across teardown, so later
builds are incremental (minutes). `provision.sh` never wipes a formatted volume.

## Validate

```bash
# amd64 parity vs clark's tarball (must be zero surface diffs)
BROWSER_BINARY_PATH=/work/build/src/out/x64/chrome \
  BROWSER_FONTS_DIR=/opt/winfonts \
  python3 packages/browser/validate/parity.py

# per-persona smoke
SMOKE_PROFILE=windows BROWSER_FONTS_DIR=/opt/winfonts \
  BROWSER_BINARY_PATH=/work/build/src/out/x64/chrome \
  python3 packages/browser/validate/smoke.py
SMOKE_PROFILE=macos \
  BROWSER_BINARY_PATH=/path/to/arm64/chrome \
  python3 packages/browser/validate/smoke.py   # runs on an arm64 host
```

amd64 is validated by cross-binary parity (a clark reference exists). arm64 has
no clark reference, so it is validated by internal coherence only (macOS-persona
smoke: `architecture == "arm"`, frozen `Intel Mac OS X 10_15_7` UA, no
`HeadlessChrome` token, single-source brand version).

## Version bumps and the fork-and-diverge contract

We forked clark's patch series and now own it. We do NOT continuously re-pull.
To bump Chromium:

1. Set `CHROMIUM_VERSION` + `UC_TAG` in `versions.env` to the new ungoogled tag.
2. Re-apply the series against the new tree; fix line-number drift patch by patch
   (the build hard-fails on a patch that doesn't apply cleanly).
3. Rebuild both targets on the warm volume; re-run parity/smoke.
4. Publish a new GitHub release, pin the new shas in `versions.env`, bump the
   Dockerfile.

**stealth5 delta.** Our 24-patch series is exactly clark's stealth5 release -
the binary cuttle ships today - so parity is provable. clark later added two
patches (`0027-analyser-node-noise`, `0047-suppress-cdc-globals`) that are NOT
in our series. They are a real stealth improvement available to cherry-pick for
a shipping build; doing so is a deliberate divergence that breaks stealth5
parity by design, so add them only after the zero-diff parity gate is recorded.

## macOS-persona fonts (arm64 image, local on a Mac)

The arm64 image needs a coherent Mac font list. Bind-mount the host Mac's
pristine system set read-only - `/System/Library/Fonts` (+ `.../Supplemental`),
NOT `~/Library/Fonts` (user fonts add entropy) - and point
`--fingerprint-fonts-dir` there. Ships no fonts (the user's own OS fonts, local
only). Docker Desktop: add `/System` under Settings -> Resources -> File sharing,
or copy the set into a shared dir and mount that.

## Widevine / EME

amd64: enable via a separate shipping-args overlay + sideload Google's linux-x64
CDM (after parity is recorded). arm64: deferred research spike -
`docs/plans/2607-23-arm64-widevine-spike.md`.
