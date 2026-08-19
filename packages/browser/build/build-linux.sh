#!/usr/bin/env bash
# Build the stealth-Chromium binary for Linux inside the build container.
#
# Adapted from clark-browser build/build-linux.sh (MIT, clark-labs-inc) and
# owned here: the x64 lane is byte-faithful to clark's method so our amd64
# binary reaches behavioral parity with clark's published tarball; the arm64
# lane (x64-host -> arm64-target) is our own addition.
#
# TARGET_CPU selects the target: x64 (default, Windows persona downstream) or
# arm64 (macOS persona downstream). The build HOST is always linux/amd64.
#
# Mount points (created by run-build.sh on host):
#   /work          - persistent build dir (~80 GB src + out/<cpu> + sccache)
#   /patches       - read-only patch series (packages/browser/patches)
#   /out           - release artifacts (stealth-chromium-linux-<cpu>.tar.gz)
#
# Exit code is the build's exit code. Re-running from a partial state is safe.
set -euo pipefail

WORK="${BROWSER_WORK_DIR:-/work}"
PATCHES="/patches"
OUT="/out"
PYTHON=$(command -v python3)
TARGET_CPU="${TARGET_CPU:-x64}"

pip_install() {
  python3 -m pip install --quiet "$@" || \
    python3 -m pip install --quiet --break-system-packages "$@"
}

case "$TARGET_CPU" in
  x64|arm64) ;;
  amd64) TARGET_CPU="x64" ;;
  aarch64) TARGET_CPU="arm64" ;;
  *)
    echo "[browser-build] unsupported TARGET_CPU=$TARGET_CPU (want x64|arm64)" >&2
    exit 2
    ;;
esac

# The build host is always linux/amd64 (Hetzner CCX). The x64 target compiles
# natively; the arm64 target cross-compiles via Chromium's own x64-host ->
# arm64-target toolchain (declared natively, unlike the arm64-host reverse
# that clark had to hand-add).
# The build host is always linux/amd64: the arm64 target cross-compiles. Anything
# else fails later anyway (the node toolchain path below is x64-only).
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64|amd64) HOST_ARCH="amd64"; CIPD_PLAT="linux-amd64" ;;
  *) echo "[browser-build] unsupported host arch: $HOST_ARCH (build host must be amd64)" >&2; exit 1 ;;
esac
echo "[browser-build] host=$HOST_ARCH target_cpu=$TARGET_CPU (cipd: $CIPD_PLAT)"

OUT_DIR="out/${TARGET_CPU}"

# sccache: cache dir on the mounted volume so it survives a src git-clean and
# speeds cross-target + post-bump rebuilds. Transparent to compiler output
# (object files are identical), so it does not affect behavioral parity.
export SCCACHE_DIR="${SCCACHE_DIR:-$WORK/sccache}"
export SCCACHE_CACHE_SIZE="${SCCACHE_CACHE_SIZE:-150G}"
mkdir -p "$SCCACHE_DIR"
USE_SCCACHE=0
if [[ "${BROWSER_NO_SCCACHE:-0}" != "1" ]] && command -v sccache >/dev/null 2>&1; then
  USE_SCCACHE=1
  sccache --start-server >/dev/null 2>&1 || true
  echo "[browser-build] sccache: $(command -v sccache) dir=$SCCACHE_DIR cap=$SCCACHE_CACHE_SIZE"
fi

echo "[browser-build] work=$WORK patches=$PATCHES out=$OUT out_dir=$OUT_DIR"
# NOTE: do not try to verify $WORK here. run-build.sh bind-mounts it, and a bind
# mount is always a mountpoint inside the container, so `mountpoint -q /work`
# is true whether or not the host volume is mounted. The check lives on the HOST
# in run-build.sh, which is the only place it can actually fail.
mkdir -p "$WORK" "$OUT"

cd "$WORK"

# Stage 1: clone ungoogled-chromium pinned to the exact tag ---------------------
UC_TAG="${BROWSER_UC_TAG:?set BROWSER_UC_TAG (run-build.sh passes UC_TAG from versions.env)}"
if [[ ! -d ungoogled-chromium ]]; then
  echo "[browser-build] Cloning ungoogled-chromium @ ${UC_TAG}..."
  git clone --depth=1 --branch "$UC_TAG" \
    https://github.com/ungoogled-software/ungoogled-chromium.git || \
  git clone https://github.com/ungoogled-software/ungoogled-chromium.git
  (cd ungoogled-chromium && git checkout "$UC_TAG")
fi
# The checkout is reused across runs on the warm volume, so a versions.env bump
# must not silently rebuild the previous source under the new release name.
UC_HEAD="$(cd ungoogled-chromium && git describe --tags --exact-match 2>/dev/null || git -C ungoogled-chromium rev-parse HEAD)"
if [[ "$UC_HEAD" != "$UC_TAG" ]]; then
  echo "[browser-build] ungoogled-chromium is at '$UC_HEAD', pinned tag is '$UC_TAG'; re-checking out..." >&2
  # -f discards our own in-place clone.py edit (the gsutil defang below). Without
  # it git refuses the checkout ("local changes would be overwritten"), which made
  # every version bump fail on a warm volume.
  (cd ungoogled-chromium && git fetch --tags --depth=1 origin "$UC_TAG" && git checkout -f "$UC_TAG")
  rm -f build/src/.ungoogled-applied
fi

# Defang clone.py: comment out the gsutil submodule update step (the recursive
# update against pinned commits hangs on httplib2; the chromium build never
# invokes gsutil). Set BROWSER_NO_CLONE_PATCH=1 to skip on native Linux hosts
# where the recursive update runs fine.
if [[ "${BROWSER_NO_CLONE_PATCH:-0}" != "1" ]] && ! grep -q 'BROWSER_PATCHED_GSUTIL_SKIP' ungoogled-chromium/utils/clone.py; then
  echo "[browser-build] Patching clone.py to skip gsutil submodule update..."
  python3 - <<'PYEOF'
import re
from pathlib import Path
p = Path('ungoogled-chromium/utils/clone.py')
text = p.read_text()
pattern = re.compile(
    r"run\(\[\s*'git',\s*'submodule',\s*'update',\s*'--init',\s*'--recursive'.*?\)",
    re.DOTALL,
)
m = pattern.search(text)
assert m, "clone.py shape changed; cannot patch"
text = text[:m.start()] + (
    "pass  # BROWSER_PATCHED_GSUTIL_SKIP: skipped recursive submodule fetch.\n"
    "    # The original recursive submodule update against pinned commits hangs\n"
    "    # on httplib2; the chromium build never invokes gsutil so it is unneeded."
) + text[m.end():]
p.write_text(text)
PYEOF
fi

# Stage 2: fetch chromium source via clone.py -----------------------------------
if [[ ! -d build/src/chrome ]]; then
  echo "[browser-build] Cloning Chromium source (30-60 min)..."
  mkdir -p build
  if ! "$PYTHON" ungoogled-chromium/utils/clone.py -p linux -o "$PWD/build/src"; then
    if [[ ! -d build/src/chrome ]]; then
      echo "[browser-build] clone.py failed before Chromium source was available" >&2
      exit 2
    fi
    echo "[browser-build] clone.py failed after source checkout; continuing to recovery sync..."
  fi
fi

# Stage 2b: recover a partial clone where gclient sync didn't fully materialise
# third_party/*. Detect via known third_party files, then re-run gclient sync
# with FULL history.
if [[ ! -f build/src/third_party/angle/dotfile_settings.gni ]] \
   || [[ ! -f build/src/v8/gni/v8.gni ]] \
   || [[ ! -f build/src/third_party/skia/BUILD.gn ]] \
   || [[ ! -f build/src/third_party/node/node_modules/lit-html/directives/repeat.d.ts ]]; then
  echo "[browser-build] Recovering missing chromium DEPS via gclient sync..."
  (cd build/src && git checkout -- . 2>/dev/null && git clean -fdx -e uc_staging -e .browser-applied -e .ungoogled-applied 2>/dev/null) || true
  find build/src -path '*/.git/index.lock' -delete 2>/dev/null || true
  rm -f build/src/.browser-applied/* build/src/.ungoogled-applied 2>/dev/null || true
  cat > build/src/uc_staging/.gclient <<GCEOF
solutions = [
  {
    "name": "${PWD}/build/src",
    "url": "https://chromium.googlesource.com/chromium/src.git",
    "managed": False,
    "custom_deps": {
      "${PWD}/build/src/third_party/angle/third_party/VK-GL-CTS/src": None,
    },
    "custom_vars": {
      "checkout_configuration": "small",
      "non_git_source": "False",
    },
  },
];
target_os = ['unix'];
target_os_only = True;
target_cpu = ['${TARGET_CPU}'];
target_cpu_only = False;
GCEOF
  DT="$PWD/build/src/uc_staging/depot_tools"
  bash "$DT/cipd_bin_setup.sh"
  export PATH="$DT:$PATH"
  GSUTIL_VENV="$WORK/.browser-gsutil-venv"
  if [[ ! -x "$GSUTIL_VENV/bin/gsutil" ]]; then
    "$PYTHON" -m venv "$GSUTIL_VENV"
    "$GSUTIL_VENV/bin/python" -m pip install --quiet "gsutil==5.35"
  fi
  SYSTEM_GSUTIL="$GSUTIL_VENV/bin/gsutil"
  python3 - "$DT/download_from_google_storage.py" "$SYSTEM_GSUTIL" <<'PY'
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
system_gsutil = sys.argv[2]
text = path.read_text()
text = re.sub(
    r"GSUTIL_DEFAULT_PATH = os\.path\.join\([^\n]+\n\s+'gsutil\.py'\)",
    f"GSUTIL_DEFAULT_PATH = {system_gsutil!r}",
    text,
    count=1,
)
text = text.replace("cmd = [self.VPYTHON3, self.path]", "cmd = [self.path]")
path.write_text(text)
print(f"download_from_google_storage.py: GSUTIL_DEFAULT_PATH={system_gsutil}, direct_exec=True")
PY
  GCLIENT_OK=0
  for attempt in 1 2 3 4 5; do
    find build/src -path '*/.git/index.lock' -delete 2>/dev/null || true
    if (cd build/src/uc_staging && \
         DEPOT_TOOLS_UPDATE=0 PYTHONDONTWRITEBYTECODE=1 \
         PATH="$DT:$PATH" \
         ./depot_tools/gclient sync -f -D -R --nohooks --sysroot=None \
                                    --jobs=2); then
      GCLIENT_OK=1
      break
    fi
    sleep_for=$((attempt * 30))
    echo "[browser-build] gclient sync attempt $attempt failed; sleeping ${sleep_for}s..."
    sleep "$sleep_for"
  done
  if [[ "$GCLIENT_OK" != "1" ]]; then
    echo "[browser-build] gclient sync failed after retries" >&2
    exit 3
  fi
fi

# Stage 3: apply ungoogled patches ----------------------------------------------
if [[ ! -f build/src/.ungoogled-applied ]]; then
  echo "[browser-build] Resetting source tree to clean state..."
  # `git checkout -- .` silently failed to revert the ~660 files the ungoogled
  # series touches (its errors were swallowed), so a re-apply hit "previously
  # applied" and stage 3 aborted - i.e. the tree could never be re-prepared.
  # reset --hard does it in ~2s. clean -fd (deliberately NOT -x) drops the files
  # ungoogled ADDS without deleting the ~19GB of gclient-managed third_party;
  # -e uc_staging keeps depot_tools, matching what clone.py itself preserves.
  # Three ungoogled patches edit files inside git SUBMODULES (v8,
  # third_party/devtools-frontend/src). A top-level reset does not reach into
  # those, so their edits survived and the re-apply reported "previously
  # applied" - which made the tree impossible to re-prepare. 266 submodules,
  # ~5s to reset them all.
  (cd build/src \
     && git reset --hard HEAD \
     && git submodule foreach --quiet 'git reset --hard -q 2>/dev/null; git clean -qfd 2>/dev/null' \
     && git clean -fd -e uc_staging) || true
  echo "[browser-build] Applying ungoogled-chromium patch series..."
  cd build/src
  set +e
  failed=()
  for p in $(cat ../../ungoogled-chromium/patches/series); do
    # -F3 here, -F0 for OUR series below: ungoogled's patches are upstream-
    # authored for this exact tag and three of them genuinely need fuzz, while a
    # mislanded hunk in OUR stealth series is the failure we must never ship.
    if ! patch -p1 --batch --forward --no-backup-if-mismatch -F3 \
        < "../../ungoogled-chromium/patches/$p" > /tmp/patch.log 2>&1; then
      failed+=("$p")
      echo "[browser-build]   WARN: ungoogled patch failed: $p"
      head -5 /tmp/patch.log | sed 's/^/[browser-build]     /'
    fi
  done
  set -e
  if (( ${#failed[@]} )); then
    echo "[browser-build] ERROR: ${#failed[@]} ungoogled patch(es) failed to apply:" >&2
    printf '[browser-build]   %s\n' "${failed[@]}" >&2
    echo "[browser-build] A partially patched tree would still build and be packaged" >&2
    echo "[browser-build] as a valid artifact, so refuse to continue." >&2
    exit 2
  fi
  echo "[browser-build] ungoogled series applied cleanly"
  touch .ungoogled-applied
  cd ../..
fi

# Stage 4: apply our patch series -----------------------------------------------
# Patches with actual diff content MUST apply. Spec-only patches (no
# `diff --git` block) are inert placeholders - skip with a note.
echo "[browser-build] Applying stealth patch series..."
cd build/src
# nullglob so an empty /patches (cache-warming build with no stealth series)
# skips this loop instead of passing the literal glob to sha256sum and
# tripping set -e.
shopt -s nullglob
for p in "$PATCHES"/0*.patch; do
  name=$(basename "$p")
  # Key the marker on the patch's CONTENT: a filename-keyed marker makes an edited
  # patch silently skip on the warm tree, shipping a binary without the change.
  phash=$(sha256sum "$p" | cut -c1-16)
  if [[ -f ".browser-applied/$name.$phash.done" ]]; then continue; fi
  rm -f ".browser-applied/$name."*.done
  if ! grep -q '^diff --git' "$p"; then
    echo "[browser-build]   $name (spec-only; skipping)"
    mkdir -p .browser-applied && touch ".browser-applied/$name.$phash.done"
    continue
  fi
  echo "[browser-build]   $name"
  if patch -p1 --batch --forward --no-backup-if-mismatch -F0 < "$p"; then
    mkdir -p .browser-applied && touch ".browser-applied/$name.$phash.done"
  else
    echo "[browser-build] FAILED to apply patch: $name" >&2
    exit 2
  fi
done

# Stage 5: drop in the 000-shared headers + sources -----------------------------
if [[ -d "$PATCHES/000-shared" ]]; then
  echo "[browser-build] Copying 000-shared files into source tree..."
  for f in clark_fingerprint_switches.h clark_fingerprint_switches.cc clark_seed.h clark_seed.cc; do
    cp -fv "$PATCHES/000-shared/$f" third_party/blink/common/ 2>/dev/null || true
  done
  mkdir -p chrome/common
  cp -fv "$PATCHES/000-shared/clark_seed.h" chrome/common/ 2>/dev/null || true
  cp -fv "$PATCHES/000-shared/clark_fingerprint_switches.h" chrome/common/ 2>/dev/null || true

  GN_FILE=third_party/blink/common/BUILD.gn
  if ! grep -q "clark_seed.cc" "$GN_FILE"; then
    python3 - <<'PY'
import pathlib
p = pathlib.Path("third_party/blink/common/BUILD.gn")
s = p.read_text()
needle = 'sources = ['
i = s.find(needle)
if i < 0:
    raise SystemExit("BUILD.gn: no sources = [ block found")
nl = s.find('\n', i)
inject = (
    '\n    "clark_fingerprint_switches.cc",'
    '\n    "clark_fingerprint_switches.h",'
    '\n    "clark_seed.cc",'
    '\n    "clark_seed.h",'
)
p.write_text(s[:nl] + inject + s[nl:])
print("BUILD.gn: clark sources wired into blink_common target")
PY
  fi
fi
cd ../..

# Stage 6: build ----------------------------------------------------------------
echo "[browser-build] Building (multi-hour step)..."
cd build/src
mkdir -p "$OUT_DIR"
: > "$OUT_DIR/args.gn"
# ungoogled's own flags.gn FIRST: its patch series is authored against these and
# silently miscompiles without them. enable_service_discovery=false is load-
# bearing - fix-building-without-mdns-and-service-discovery.patch strips
# service_discovery_client_ from dns_sd_registry.h, so leaving the flag at its
# default true breaks the build ~30k targets in. Keys we deliberately override
# below are filtered out here so there is exactly one assignment per key.
UC_FLAGS="$WORK/ungoogled-chromium/flags.gn"
if [[ -f "$UC_FLAGS" ]]; then
  grep -vE '^(chrome_pgo_phase|clang_use_chrome_plugins|enable_remoting|safe_browsing_mode|treat_warnings_as_errors|enable_widevine)=' \
    "$UC_FLAGS" >> "$OUT_DIR/args.gn"
  echo "[browser-build] merged $(grep -c . "$UC_FLAGS") ungoogled flags into args.gn"
else
  echo "[browser-build] ERROR: $UC_FLAGS missing - refusing to build without ungoogled's flags" >&2
  exit 2
fi
cat >> "$OUT_DIR/args.gn" <<'GNEOF'
is_debug = false
# Keep official_build true but disable ThinLTO/CFI/PGO explicitly - the heavy
# paths clark also disables, kept identical for parity of the x64 output.
is_official_build = true
use_thin_lto = false
thin_lto_enable_optimizations = false
is_cfi = false
symbol_level = 0
blink_symbol_level = 0
v8_symbol_level = 0
enable_nacl = false
enable_remoting = false
proprietary_codecs = true
ffmpeg_branding = "Chrome"
# Widevine: unbranded Chromium defaults enable_widevine=false, so an EME query
# returns unsupported and the persona reads as Chromium rather than Chrome.
# Upstream sanctions enabling it on non-Android platforms and ungoogled's own
# flags.gn sets it. bundle_widevine_cdm stays false (not chrome-branded), so we
# compile the key-system support but ship NO proprietary blob - the CDM is
# fetched at runtime by the container, never redistributed in our artifacts.
enable_widevine = true
treat_warnings_as_errors = false
GNEOF
# Target CPU + sysroot. x64 uses the host glibc (no sysroot, gclient ran
# --nohooks). arm64 cross-compiles against the fetched arm64 sysroot.
if [[ "$TARGET_CPU" == "arm64" ]]; then
  cat >> "$OUT_DIR/args.gn" <<'GNEOF'
target_cpu = "arm64"
v8_target_cpu = "arm64"
use_sysroot = true
GNEOF
else
  cat >> "$OUT_DIR/args.gn" <<'GNEOF'
target_cpu = "x64"
use_sysroot = false
GNEOF
fi
cat >> "$OUT_DIR/args.gn" <<'GNEOF'
# Disable safe_browsing so the ungoogled fix-pruned-binaries patch can't break
# the gn build graph. Disable PGO (profiles were not fetched).
safe_browsing_mode = 0
chrome_pgo_phase = 0
GNEOF
if [[ "$USE_SCCACHE" == "1" ]]; then
  echo "cc_wrapper = \"sccache\"" >> "$OUT_DIR/args.gn"
  # cc_wrapper alone caches almost nothing on Chromium: gn's default flags make
  # sccache mark ~every compile non-cacheable (verified via `sccache --show-stats`).
  # Two flag families cause it; each is removed by a gn arg, and neither changes
  # emitted code, so the amd64 behavioral-parity gate is unaffected:
  #   clang_use_chrome_plugins -> -Xclang -add-plugin (blink-gc / find-bad-constructs
  #     style checks). sccache bails on unknown -Xclang args (UnknownFlag -> CannotCache);
  #     Chromium's own cc_wrapper.gni documents disabling it for compiler-cache users.
  #     Analysis-only, no codegen effect.
  #   use_clang_modules -> -fmodules + -Xclang -fmodule* (libc++ Clang header modules).
  #     sccache hard-codes -fmodules as TooHardFlag -> CannotCache. This declare_args is
  #     what actually gates the flags in build/config/compiler/BUILD.gn - NOT
  #     use_libcxx_modules, which is only a per-target dep var (setting that was a no-op).
  #     Chromium already force-disables modules for reclient and cc_wrapper==icecc
  #     ("don't handle headers in modulemap config"); sccache is the same case, just not
  #     in their exclusion list, so we set it explicitly. Header modules are a
  #     semantically-transparent parse optimization; textual includes emit identical code.
  #   use_libcxx_modules=false additionally drops the now-unused libc++ modulemap deps.
  # (is_cfi/use_thin_lto/chrome_pgo_phase are already off above - they would otherwise
  # also hurt cacheability.) Result: ~every compile is cacheable, so a warm
  # /work/sccache turns a from-scratch rebuild into minutes. See build/README.md.
  echo "clang_use_chrome_plugins = false" >> "$OUT_DIR/args.gn"
  echo "use_clang_modules = false" >> "$OUT_DIR/args.gn"
  echo "use_libcxx_modules = false" >> "$OUT_DIR/args.gn"
fi

DT="$PWD/uc_staging/depot_tools"
GN_REV=$(grep "'gn_version'" "$PWD/DEPS" | sed -E "s/.*git_revision:([a-f0-9]+).*/\1/" | head -1)
echo "[browser-build] Ensuring gn pin git_revision:$GN_REV is installed..."
if [[ ! -x buildtools/linux64/gn ]]; then
  mkdir -p buildtools/linux64
  "$DT/cipd" install "gn/gn/${CIPD_PLAT}" "git_revision:$GN_REV" \
    -root buildtools/linux64 2>&1 | tail -3
fi
GN_BIN="$PWD/buildtools/linux64/gn"
"$GN_BIN" --version

# Dawn's source generator (third_party/dawn/tools/generate-sources-gn.py) shells
# out to a Go toolchain. Its DEPS entry is gated on `non_git_source`, which our
# recovery .gclient sets to False, so gclient never fetches it and the build dies
# at //third_party/dawn/src/tint:generate_sources with FileNotFoundError on
# .../tools/golang/linux-amd64/bin/go. The generator runs on the HOST, so only
# linux-amd64 is needed regardless of TARGET_CPU. Version is read from the tree's
# own DEPS so it cannot drift from the pinned Chromium.
GO_ROOT_REL="third_party/dawn/tools/golang/linux-amd64"
if [[ ! -x "$GO_ROOT_REL/bin/go" ]]; then
  DAWN_GO_VER=$(grep -E "'dawn_go_version'" third_party/dawn/DEPS \
                | sed -E "s/.*'(version:[^']+)'.*/\1/" | head -1)
  if [[ -z "$DAWN_GO_VER" ]]; then
    echo "[browser-build] could not read dawn_go_version from third_party/dawn/DEPS" >&2
    exit 2
  fi
  echo "[browser-build] Installing Dawn Go toolchain ($DAWN_GO_VER)..."
  mkdir -p "$GO_ROOT_REL"
  "$DT/cipd" install "infra/3pp/tools/go/linux-amd64" "$DAWN_GO_VER" \
    -root "$GO_ROOT_REL" 2>&1 | tail -3
fi
"$GO_ROOT_REL/bin/go" version

# Stub gclient_args.gni - normally written by gclient sync runhooks (skipped
# via --nohooks). Always re-write so newly-required keys get picked up.
cat > build/config/gclient_args.gni <<'GNIEOF'
# Stubbed by build-linux.sh because gclient ran with --nohooks.
checkout_android = false
checkout_android_prebuilts_build_tools = false
checkout_android_native_support = false
checkout_chromium_autofill_test_dependencies = false
checkout_chromium_internal_resources = false
checkout_clusterfuzz_data = false
checkout_chromevox_dependencies = false
checkout_clang_coverage_tools = false
checkout_clang_tidy = false
checkout_clangd = false
checkout_copybara = false
checkout_cros_internal = false
checkout_fuchsia = false
checkout_fuchsia_for_arm64_host = false
checkout_fuchsia_internal = false
checkout_glic = false
checkout_glic_e2e_tests = false
checkout_glic_internal = false
checkout_ios = false
checkout_ios_webkit = false
checkout_libaom_testdata = false
checkout_libvpx_testdata = false
checkout_lottie_proprietary_tests = false
checkout_mac_sdk = false
checkout_mutter = false
checkout_nacl = false
checkout_openxr = false
checkout_oculus_sdk = false
checkout_optimization_profiles = false
checkout_pgo_profiles = false
checkout_remoteexec = false
checkout_rts_model = false
checkout_src_internal = false
checkout_telemetry_dependencies = false
checkout_test_data = false
checkout_traffic_annotation_tools = false
checkout_webp_dirs = false
build_with_chromium = true
cros_boards = ""
cros_boards_with_qemu_images = ""
generate_location_tags = true
non_git_source = false
GNIEOF

if [[ ! -f build/util/LASTCHANGE ]]; then
  echo "LASTCHANGE=$(date +%Y-%m-%dT%H:%M:%S)-stub" > build/util/LASTCHANGE
  date +%s > build/util/LASTCHANGE.committime
fi

echo "[browser-build] Fetching prebuilt toolchains (rust, clang, node)..."
[[ -f third_party/rust-toolchain/VERSION ]] || python3 tools/rust/update_rust.py
[[ -d third_party/llvm-build/Release+Asserts/bin ]] || \
  python3 tools/clang/scripts/update.py
[[ -x third_party/node/linux/node-linux-x64/bin/node ]] || \
  bash third_party/node/update_node_binaries

[[ -x third_party/gperf/cipd/bin/gperf ]] || \
  "$DT/cipd" install "infra/3pp/tools/gperf/${CIPD_PLAT}" "version:3@3.2" \
    -root third_party/gperf/cipd 2>&1 | tail -3

mkdir -p gpu/webgpu
if [[ ! -f gpu/webgpu/DAWN_VERSION ]]; then
  python3 build/util/lastchange.py \
    -m DAWN_COMMIT_HASH \
    -s third_party/dawn \
    --revision gpu/webgpu/DAWN_VERSION \
    --header gpu/webgpu/dawn_commit_hash.h
fi
if [[ ! -f gpu/config/gpu_lists_version.h ]]; then
  printf '#define GPU_LISTS_VERSION "0000000000000000000000000000000000000000"\n' \
    > gpu/config/gpu_lists_version.h
fi
if [[ ! -f skia/ext/skia_commit_hash.h ]]; then
  mkdir -p skia/ext
  printf '#define SKIA_COMMIT_HASH "0000000000000000000000000000000000000000"\n' \
    > skia/ext/skia_commit_hash.h
fi
if [[ ! -f skia/skia_commit_hash.h ]]; then
  mkdir -p skia
  printf '#define SKIA_COMMIT_HASH "0000000000000000000000000000000000000000"\n' \
    > skia/skia_commit_hash.h
fi

if [[ ! -f /tmp/.browser-build-deps-installed ]]; then
  echo "[browser-build] Running chromium install-build-deps.sh..."
  # arm64 target needs --arm to pull cross libs; x64 keeps --no-arm.
  ARM_FLAG="--no-arm"
  [[ "$TARGET_CPU" == "arm64" ]] && ARM_FLAG="--arm"
  yes | bash build/install-build-deps.sh \
    --no-prompt --no-chromeos-fonts --no-nacl "$ARM_FLAG" 2>&1 | tail -8 || true
  touch /tmp/.browser-build-deps-installed
fi
if [[ ! -f buildtools/linux64/clang-format ]]; then
  CF_REV=$(grep "'clang-format'" "$PWD/buildtools/DEPS" 2>/dev/null \
    | sed -E "s/.*git_revision:([a-f0-9]+).*/\1/" | head -1 || true)
  if [[ -n "$CF_REV" && "$CF_REV" =~ ^[a-f0-9]+$ ]]; then
    "$DT/cipd" install "fuchsia/third_party/clang-format/${CIPD_PLAT}" \
      "git_revision:$CF_REV" -root buildtools/linux64 2>&1 | tail -3 || true
  fi
fi

# Sysroots for the arm64 cross-compile. use_sysroot=true applies to BOTH
# toolchains, so we need the arm64 TARGET sysroot AND the amd64 HOST sysroot -
# the host clang_x64 toolchain (protoc and other build-time host tools) asserts
# the amd64 sysroot exists during gn gen. Installing only arm64 fails with
# "Missing sysroot (debian_bullseye_amd64-sysroot)".
if [[ "$TARGET_CPU" == "arm64" ]]; then
  echo "[browser-build] Installing arm64 (target) + amd64 (host) sysroots for cross-compile..."
  python3 build/linux/sysroot_scripts/install-sysroot.py --arch=arm64 2>&1 | tail -5 || true
  python3 build/linux/sysroot_scripts/install-sysroot.py --arch=amd64 2>&1 | tail -5 || true
fi

"$GN_BIN" gen "$OUT_DIR"
echo "[browser-build] Ninja target: chrome (cpu=$TARGET_CPU)"
ninja -C "$OUT_DIR" -j "$(nproc)" chrome

[[ "$USE_SCCACHE" == "1" ]] && sccache --show-stats || true

# Stage 7: package --------------------------------------------------------------
echo "[browser-build] Packaging..."
cd "$OUT_DIR"
PACKAGE_FILES=()
add_package_file() {
  local path="$1"
  if [[ -e "$path" ]]; then
    local existing
    for existing in "${PACKAGE_FILES[@]}"; do
      [[ "$existing" == "$path" ]] && return
    done
    PACKAGE_FILES+=("$path")
  fi
}
add_package_glob() {
  local pattern="$1" match
  shopt -s nullglob
  for match in $pattern; do add_package_file "$match"; done
  shopt -u nullglob
}

add_package_file chrome
for optional in \
  chrome_crashpad_handler chrome_sandbox \
  headless_command_resources.pak headless_lib_data.pak headless_lib_strings.pak \
  resources.pak chrome_100_percent.pak chrome_200_percent.pak \
  libEGL.so libGLESv2.so libvulkan.so.1 libvk_swiftshader.so \
  vk_swiftshader_icd.json v8_context_snapshot.bin snapshot_blob.bin \
  icudtl.dat locales; do
  [[ -e "$optional" ]] && add_package_file "$optional"
done
add_package_glob "*.bin"
add_package_glob "*.json"
add_package_glob "*.pak"
add_package_glob "*.so"
add_package_glob "*.so.*"

# Stage 7b: smoke the BINARY before packaging ----------------------------------
# The smoke must gate the artifact, so it runs before tar/sha256: a published
# .sha256 is the thing ops/docker/Dockerfile pins, and an artifact that exists
# only after its gate passed cannot be shipped by mistake. x64 only - an arm64
# binary cannot execute on the amd64 build host.
if [[ "${BROWSER_SKIP_SMOKE:-0}" != "1" && "$TARGET_CPU" == "x64" ]]; then
  echo "[browser-build] Stage 7b: in-container smoke test"
  pip_install websocket-client 2>&1 | tail -3 || true
  SMOKE_SCRIPT="${BROWSER_SMOKE_SCRIPT:-$WORK/packages/browser/validate/smoke.py}"
  if [[ ! -f "$SMOKE_SCRIPT" ]]; then
    echo "[browser-build] ERROR: smoke.py not found at $SMOKE_SCRIPT." >&2
    echo "[browser-build] run-build.sh mounts it; a missing mount would turn the" >&2
    echo "[browser-build] gate into a silent no-op. Set BROWSER_SKIP_SMOKE=1 to skip." >&2
    exit 2
  fi
  # smoke.py derives the persona from the binary's arch (x64 -> windows), so it
  # needs no SMOKE_PROFILE here. The font packs live in the image, not on this
  # host, so pass BROWSER_FONTS_DIR only if one was staged.
  SMOKE_ENV=(BROWSER_BINARY_PATH="$WORK/build/src/$OUT_DIR/chrome")
  [[ -n "${BROWSER_FONTS_DIR:-}" ]] && SMOKE_ENV+=(BROWSER_FONTS_DIR="$BROWSER_FONTS_DIR")
  env "${SMOKE_ENV[@]}" python3 "$SMOKE_SCRIPT" || {
    echo "[browser-build] SMOKE FAILED - refusing to package $WORK/build/src/$OUT_DIR/chrome" >&2
    exit 1
  }
  echo "[browser-build] Smoke passed."
elif [[ "$TARGET_CPU" != "x64" ]]; then
  echo "[browser-build] NOTE: $TARGET_CPU cannot be smoked on this amd64 host - the"
  echo "[browser-build] artifact is UNGATED here; validate it after the image build."
fi

cd "$WORK/build/src/$OUT_DIR"
ARTIFACT="$OUT/stealth-chromium-linux-${TARGET_CPU}.tar.gz"
tar -czf "$ARTIFACT" "${PACKAGE_FILES[@]}"
echo "[browser-build] Done. Artifact: $ARTIFACT"
ls -lh "$ARTIFACT"
sha256sum "$ARTIFACT" | tee "$OUT/stealth-chromium-linux-${TARGET_CPU}.tar.gz.sha256"
