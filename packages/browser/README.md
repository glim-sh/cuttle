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
  00NN-*.patch    25 patches; applied at -F0 (see "Version bumps" below)
build/
  Dockerfile.linux  ubuntu:24.04 build image + pinned sccache
  build-linux.sh    parametrized by TARGET_CPU=x64|arm64; sccache-cached
  run-build.sh      docker driver on the Hetzner host (persistent /work volume)
hetzner/
  cloud-init.yaml   installs docker, mounts the cache volume at /work
  provision.sh      hcloud: create volume + server (idempotent, warm-cache safe)
  teardown.sh       hcloud: delete server, KEEP volume
validate/
  smoke.py          per-persona behavioral smoke (windows|macos)
  parity.py         surface diff vs our own previous release (delta report,
                    no longer a gate - see "Validate")
  report.md         (generated) coherence + delta results
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
```

Both targets are now validated by internal coherence (macOS-persona smoke:
`architecture == "arm"`, frozen `Intel Mac OS X 10_15_7` UA, no `HeadlessChrome`
token, single-source brand version), plus the surface delta against our own
previous release.

## Version bumps and the fork-and-diverge contract

We forked clark's patch series and now own it. We do NOT continuously re-pull.
To bump Chromium:

1. Set `CHROMIUM_VERSION` + `UC_TAG` in `versions.env` to the new ungoogled tag.
2. Re-apply the series against the new tree; fix drift patch by patch (the build
   hard-fails on a patch that doesn't apply cleanly).
3. Rebuild both targets; re-run smoke + the delta report.
4. Publish a new GitHub release, pin the new shas in `versions.env`, bump the
   Dockerfile.

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
