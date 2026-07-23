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
BROWSER_TARGET="${BROWSER_TARGET:-chrome}"
TARGET_CPU="${TARGET_CPU:-x64}"

pip_install() {
  python3 -m pip install --quiet "$@" || \
    python3 -m pip install --quiet --break-system-packages "$@"
}

case "$BROWSER_TARGET" in
  headless|headless_shell) BROWSER_TARGET="headless_shell" ;;
  chrome) BROWSER_TARGET="chrome" ;;
  *)
    echo "[browser-build] unsupported BROWSER_TARGET=$BROWSER_TARGET" >&2
    echo "[browser-build] supported targets: headless_shell, chrome" >&2
    exit 2
    ;;
esac

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
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64|amd64)  HOST_ARCH="amd64"; CIPD_PLAT="linux-amd64" ;;
  aarch64|arm64) HOST_ARCH="arm64"; CIPD_PLAT="linux-arm64" ;;
  *) echo "[browser-build] unsupported host arch: $HOST_ARCH" >&2; exit 1 ;;
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

# When the /work volume moves between host arches, wipe arch-specific host
# toolchains so the script re-fetches matching ones. (We only build on amd64
# hosts, but keep clark's guard.)
if [[ -f "$WORK/build/src/buildtools/linux64/gn" ]]; then
  GN_FILE="$(file "$WORK/build/src/buildtools/linux64/gn" 2>/dev/null || true)"
  if [[ "$HOST_ARCH" == "amd64" && "$GN_FILE" != *"x86-64"* ]]; then
    echo "[browser-build] host toolchain mismatch; resetting host toolchains..."
    rm -rf "$WORK/build/src/buildtools/linux64" \
           "$WORK/build/src/third_party/llvm-build" \
           "$WORK/build/src/third_party/rust-toolchain"
  fi
fi

echo "[browser-build] work=$WORK patches=$PATCHES out=$OUT target=$BROWSER_TARGET out_dir=$OUT_DIR"
mkdir -p "$WORK" "$OUT"

cd "$WORK"

# Stage 1: clone ungoogled-chromium pinned to the exact tag ---------------------
UC_TAG="${BROWSER_UC_TAG:-148.0.7778.96-1}"
if [[ ! -d ungoogled-chromium ]]; then
  echo "[browser-build] Cloning ungoogled-chromium @ ${UC_TAG}..."
  git clone --depth=1 --branch "$UC_TAG" \
    https://github.com/ungoogled-software/ungoogled-chromium.git || \
  git clone https://github.com/ungoogled-software/ungoogled-chromium.git
  (cd ungoogled-chromium && git checkout "$UC_TAG" 2>/dev/null || true)
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
  (cd build/src && git checkout -- . 2>/dev/null && git clean -fd 2>/dev/null) || true
  echo "[browser-build] Applying ungoogled-chromium patch series..."
  cd build/src
  set +e
  failed=()
  for p in $(cat ../../ungoogled-chromium/patches/series); do
    if ! patch -p1 --batch --forward --no-backup-if-mismatch -F3 \
        < "../../ungoogled-chromium/patches/$p" > /tmp/patch.log 2>&1; then
      failed+=("$p")
      echo "[browser-build]   WARN: ungoogled patch failed: $p"
      head -5 /tmp/patch.log | sed 's/^/[browser-build]     /'
    fi
  done
  set -e
  echo "[browser-build] ungoogled series done; ${#failed[@]} patch(es) skipped"
  touch .ungoogled-applied
  cd ../..
fi

# Stage 4: apply our patch series -----------------------------------------------
# Patches with actual diff content MUST apply. Spec-only patches (no
# `diff --git` block) are inert placeholders - skip with a note.
echo "[browser-build] Applying stealth patch series..."
cd build/src
for p in "$PATCHES"/0*.patch; do
  name=$(basename "$p")
  if [[ -f ".browser-applied/$name.done" ]]; then continue; fi
  if ! grep -q '^diff --git' "$p"; then
    echo "[browser-build]   $name (spec-only; skipping)"
    mkdir -p .browser-applied && touch ".browser-applied/$name.done"
    continue
  fi
  echo "[browser-build]   $name"
  if patch -p1 --batch --forward --no-backup-if-mismatch -F3 < "$p"; then
    mkdir -p .browser-applied && touch ".browser-applied/$name.done"
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
if [[ "$BROWSER_TARGET" == "headless_shell" ]]; then
  cat > "$OUT_DIR/args.gn" <<'GNEOF'
import("//build/args/headless.gn")
GNEOF
else
  : > "$OUT_DIR/args.gn"
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
echo "[browser-build] Ninja target: $BROWSER_TARGET (cpu=$TARGET_CPU)"
ninja -C "$OUT_DIR" -j "$(nproc)" "$BROWSER_TARGET"

[[ "$USE_SCCACHE" == "1" ]] && sccache --show-stats || true

# Stage 7: package --------------------------------------------------------------
echo "[browser-build] Packaging..."
cd "$OUT_DIR"
if [[ "$BROWSER_TARGET" == "headless_shell" ]]; then
  cat > chrome <<'SHEOF'
#!/bin/sh
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$HERE/headless_shell" "$@"
SHEOF
  chmod +x chrome
else
  cat > headless_shell <<'SHEOF'
#!/bin/sh
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$HERE/chrome" "$@"
SHEOF
  chmod +x headless_shell
fi

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
add_package_file headless_shell
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

ARTIFACT="$OUT/stealth-chromium-linux-${TARGET_CPU}.tar.gz"
tar -czf "$ARTIFACT" "${PACKAGE_FILES[@]}"
echo "[browser-build] Done. Artifact: $ARTIFACT"
ls -lh "$ARTIFACT"
sha256sum "$ARTIFACT" | tee "$OUT/stealth-chromium-linux-${TARGET_CPU}.tar.gz.sha256"

# Stage 8: in-container smoke test ---------------------------------------------
cd "$WORK/build/src"
if [[ "${BROWSER_SKIP_SMOKE:-0}" != "1" && "$TARGET_CPU" == "x64" ]]; then
  echo "[browser-build] Stage 8: in-container smoke test (x64 only; arm64 can't run on amd64 host)"
  pip_install websocket-client 2>&1 | tail -3 || true
  SMOKE_SCRIPT="${BROWSER_SMOKE_SCRIPT:-$WORK/packages/browser/validate/smoke.py}"
  if [[ -f "$SMOKE_SCRIPT" ]]; then
    BROWSER_BINARY_PATH="$WORK/build/src/$OUT_DIR/$BROWSER_TARGET" \
      python3 "$SMOKE_SCRIPT" || {
        echo "[browser-build] SMOKE FAILED - binary at $WORK/build/src/$OUT_DIR/$BROWSER_TARGET"
        exit 1
      }
    echo "[browser-build] Smoke passed."
  else
    echo "[browser-build] smoke.py not mounted at $SMOKE_SCRIPT; skipping."
  fi
fi
