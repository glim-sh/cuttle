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

# BROWSER_STAGE splits the run so both targets can build at once from one tree:
#   prep  - every write to the shared tree (source, patches, toolchains, sysroots),
#           then a .browser-prepared marker. Run once, alone.
#   build - per-target only (args.gn, gn gen, ninja, smoke, package); writes only
#           out/<cpu>, so the x64 and arm64 builds can run concurrently.
#   all   - both, in one container (default).
STAGE="${BROWSER_STAGE:-all}"
case "$STAGE" in
  all|prep|build) ;;
  *) echo "[browser-build] unsupported BROWSER_STAGE=$STAGE (want all|prep|build)" >&2; exit 2 ;;
esac
PREPARED="$WORK/build/src/.browser-prepared"
# The marker records what the tree was prepared FROM, so a build stage never
# compiles a tree prepared for another tag or another version of the series.
prep_identity() {
  echo "${BROWSER_UC_TAG:-}"
  cat "$PATCHES"/0*.patch "$PATCHES"/000-shared/* 2>/dev/null | sha256sum | cut -c1-16
}

prep_tree() {
cd "$WORK"
rm -f "$PREPARED"
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
# depot_tools reformats this assignment between releases (quote style, line
# breaks), and a silent non-match falls back to the bundled gsutil, which dies on
# a missing `six`. Match loosely and fail loudly.
text, n = re.subn(
    r"GSUTIL_DEFAULT_PATH = os\.path\.join\(.*?['\"]gsutil\.py['\"]\s*\)",
    f"GSUTIL_DEFAULT_PATH = {system_gsutil!r}",
    text,
    count=1,
    flags=re.S,
)
if n != 1 and f"GSUTIL_DEFAULT_PATH = {system_gsutil!r}" not in text:
    sys.exit("download_from_google_storage.py: GSUTIL_DEFAULT_PATH shape changed; cannot redirect gsutil")
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
         ./depot_tools/gclient sync -f -D -R --nohooks --sysroot=None); then
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
# git apply, not patch -F0: it is atomic per patch, so a failed apply leaves
# nothing half-applied behind, and it refuses a zero-context hunk that replaces
# lines. A zero-context INSERTION (`@@ -N,0 +M,K @@`) it still applies, at the end
# of the file, so those are refused up front (`just patch-lint` runs the same
# check in CI).
#
# .browser-applied/<name>.<hash>.patch is a copy of each patch as applied. A patch
# that changed or left the series is reversed from that copy, which restores every
# file it touched - including files its new version no longer touches - so a warm
# prep rewrites only those files and out/ stays warm. A later applied patch that
# shares a file with one being redone is redone too, so the tree is exactly the
# one a fresh prep would produce.
echo "[browser-build] Applying stealth patch series..."
cd build/src
# nullglob so an empty /patches (cache-warming build with no stealth series)
# skips this loop instead of passing the literal glob to sha256sum and
# tripping set -e.
shopt -s nullglob
series=("$PATCHES"/0*.patch)

# An empty series is legitimate ONLY for the cache-warming build, which mounts an
# empty /patches on purpose. Anywhere else it means the mount is missing, and
# nullglob would quietly turn that into a binary with zero stealth patches -
# which then packages, because Stage 7b is skipped for non-x64 targets and under
# BROWSER_SKIP_SMOKE. Fail closed; make the warming build say so out loud.
if [[ ${#series[@]} -eq 0 && "${BROWSER_ALLOW_EMPTY_PATCHES:-0}" != "1" ]]; then
  echo "[browser-build] FATAL: no patches found at $PATCHES - refusing to build" >&2
  echo "[browser-build] an unpatched binary. Set BROWSER_ALLOW_EMPTY_PATCHES=1" >&2
  echo "[browser-build] only for a cache-warming build." >&2
  exit 2
fi

applied=.browser-applied
mkdir -p "$applied"
phash() { sha256sum "$1" | cut -c1-16; }
patch_files() { git apply --numstat "$1" | cut -f3; }
reset_hint() {
  echo "[browser-build] Reset the tree and re-prep:" >&2
  echo "[browser-build]   rm -f build/src/.ungoogled-applied" >&2
  echo "[browser-build]   rm -rf build/src/.browser-applied" >&2
  exit 2
}

declare -A want=() dirty=()
for p in "${series[@]}"; do want[${p##*/}]=$(phash "$p"); done
names=$( (for r in "$applied"/*.patch; do r=${r##*/}; echo "${r%.*.patch}"; done
          for p in "${series[@]}"; do echo "${p##*/}"; done) | LC_ALL=C sort -u)
redo=()   # records to reverse, latest first
todo=()   # series patches to apply, in order
for name in $names; do
  recs=("$applied/$name".*.patch)
  rec=${recs[0]:-}
  cur=""
  [[ -n "${want[$name]:-}" ]] && cur="$applied/$name.${want[$name]}.patch"
  files=$( ([[ -z "$rec" ]] || patch_files "$rec"
            [[ -z "$cur" ]] || patch_files "$PATCHES/$name") | sort -u)
  hit=0
  [[ "$rec" != "$cur" ]] && hit=1
  for f in $files; do [[ -n "${dirty[$f]:-}" ]] && hit=1; done
  (( hit )) || continue
  for f in $files; do dirty[$f]=1; done
  [[ -n "$rec" ]] && redo=("$rec" "${redo[@]}")
  [[ -n "$cur" ]] && todo+=("$PATCHES/$name")
done

for rec in "${redo[@]}"; do
  echo "[browser-build]   reverse ${rec##*/}"
  if ! git apply -R "$rec"; then
    echo "[browser-build] FATAL: ${rec##*/} no longer reverses cleanly, so the tree" >&2
    echo "[browser-build] was changed outside this script." >&2
    reset_hint
  fi
  rm "$rec"
done
for p in "${todo[@]}"; do
  name=${p##*/}
  echo "[browser-build]   $name"
  if grep -nE '^@@ -[1-9][0-9]*,0 ' "$p" >&2; then
    echo "[browser-build] FATAL: $name has a zero-context insertion; regenerate it" >&2
    echo "[browser-build] with packages/browser/build/regen-patch.sh." >&2
    exit 2
  fi
  if ! git apply "$p"; then
    echo "[browser-build] FAILED to apply patch: $name" >&2
    exit 2
  fi
  cp "$p" "$applied/$name.${want[$name]}.patch"
done

# Stage 5: drop in the 000-shared headers + sources -----------------------------
# Copy only on a content change: a restamped header recompiles all ~25 includers
# on every prep. Never cp -p either - an old source mtime can hide a real edit
# from ninja.
copy_shared() {
  [[ -f "$PATCHES/000-shared/$1" ]] || return 0
  cmp -s "$PATCHES/000-shared/$1" "$2/$1" || cp -fv "$PATCHES/000-shared/$1" "$2/"
}
if [[ -d "$PATCHES/000-shared" ]]; then
  echo "[browser-build] Copying changed 000-shared files into source tree..."
  for f in cuttle_fingerprint_switches.h cuttle_fingerprint_switches.cc cuttle_seed.h cuttle_seed.cc; do
    copy_shared "$f" third_party/blink/common
  done
  mkdir -p chrome/common
  copy_shared cuttle_seed.h chrome/common
  copy_shared cuttle_fingerprint_switches.h chrome/common

  GN_FILE=third_party/blink/common/BUILD.gn
  if ! grep -q "cuttle_seed.cc" "$GN_FILE"; then
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
    '\n    "cuttle_fingerprint_switches.cc",'
    '\n    "cuttle_fingerprint_switches.h",'
    '\n    "cuttle_seed.cc",'
    '\n    "cuttle_seed.h",'
)
p.write_text(s[:nl] + inject + s[nl:])
print("BUILD.gn: clark sources wired into blink_common target")
PY
  fi
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
# A shared prep serves both targets, so it installs them regardless of TARGET_CPU.
if [[ "$TARGET_CPU" == "arm64" || "$STAGE" == "prep" ]]; then
  echo "[browser-build] Installing arm64 (target) + amd64 (host) sysroots for cross-compile..."
  python3 build/linux/sysroot_scripts/install-sysroot.py --arch=arm64 2>&1 | tail -5 || true
  python3 build/linux/sysroot_scripts/install-sysroot.py --arch=amd64 2>&1 | tail -5 || true
fi

# CIPD and GCS deps gated on `non_git_source`, which our .gclient disables: each
# release adds build inputs there (the hermetic cpython3 gn runs on, the
# typescript compiler and esbuild devtools needs, clang-format, the
# subresource-filter ruleset), and hand-installing them one build failure at a
# time does not scale. Evaluate DEPS and every recursedep's DEPS with it on, and
# ensure every missing linux package in one pass. Skipped: screen-ai is a
# proprietary Google binary, and ninja/siso/reclient would shadow the build
# tools we deliberately pin. Each dep is stamped under .browser-deps/ only after
# a complete install, so an interrupted fetch is redone rather than kept.
python3 - "$PWD" "$DT/cipd" <<'NGEOF'
import collections, hashlib, json, os, subprocess, sys, tarfile, urllib.request
src, cipd = sys.argv[1], sys.argv[2]
# Matched anywhere in the path: recursedeps carry their own copies.
skip = ("third_party/screen-ai/", "third_party/ninja/", "third_party/siso/", "buildtools/reclient/")
plat = {"${{platform}}": "linux-amd64", "${{arch}}": "amd64", "${{os}}": "linux"}


def load(path):
    g = {"Str": str}
    g["Var"] = lambda k: g["vars"][k]
    exec(open(path).read(), g)
    return g


def fetch_gcs(dep, dest):
    # What gclient does for a gcs dep: fetch each object from the public bucket,
    # verify its sha256, and unpack it if it is a tarball.
    print(f"[browser-build]   gcs {os.path.relpath(dest, src)}", flush=True)
    os.makedirs(dest, exist_ok=True)
    for o in dep["objects"]:
        url = f"https://storage.googleapis.com/{dep['bucket']}/{o['object_name']}"
        data = urllib.request.urlopen(url, timeout=300).read()
        if hashlib.sha256(data).hexdigest() != o["sha256sum"]:
            sys.exit(f"sha256 mismatch for {url}")
        # object_name can carry a bucket path ("js_code_coverage/<sha>"); gclient writes the basename.
        out = os.path.join(dest, o.get("output_file") or os.path.basename(o["object_name"]))
        with open(out, "wb") as f:
            f.write(data)
        if tarfile.is_tarfile(out):
            with tarfile.open(out) as t:
                t.extractall(dest, filter="data")
            os.remove(out)
        else:
            os.chmod(out, 0o755)


def write_stamp(stamp, spec):
    os.makedirs(os.path.dirname(stamp), exist_ok=True)
    with open(stamp, "w") as f:
        f.write(spec)


def emit(g, prefix):
    env = collections.defaultdict(bool)
    env.update({k: v for k, v in g.get("vars", {}).items() if isinstance(v, (bool, str))})
    for k in [k for k in env if k.startswith("checkout_")]:
        env[k] = False
    env.update(non_git_source=True, build_with_chromium=True, checkout_linux=True,
               checkout_x64=True, checkout_arm64=True, host_os="linux", host_cpu="x64")
    for path, dep in g.get("deps", {}).items():
        if not isinstance(dep, dict) or dep.get("dep_type") not in ("cipd", "gcs"):
            continue
        cond = dep.get("condition", "True")
        rel = os.path.join(prefix, path.removeprefix("src/"))
        if "non_git_source" not in cond or not eval(cond, {}, env) or any(k in rel + "/" for k in skip):
            continue
        stamp = os.path.join(src, ".browser-deps", rel.replace("/", "__"))
        spec = json.dumps(dep, sort_keys=True)
        if os.path.isfile(stamp) and open(stamp).read() == spec:
            continue
        if dep["dep_type"] == "gcs":
            fetch_gcs(dep, os.path.join(src, rel))
            write_stamp(stamp, spec)
            continue
        pkgs = []
        for pkg in dep["packages"]:
            name = pkg["package"]
            for k, v in plat.items():
                name = name.replace(k, v)
            pkgs.append(f"{name} {pkg['version']}\n")
        # One cipd root per dep dir: `cipd ensure` is declarative for its root
        # and REMOVES anything there not in the file, so a shared root would
        # delete every dep installed by an earlier run.
        print(f"[browser-build]   cipd ensure {rel}", flush=True)
        subprocess.run([cipd, "ensure", "-root", os.path.join(src, rel), "-ensure-file", "-"],
                       input="".join(pkgs), text=True, check=True, stdout=subprocess.DEVNULL)
        write_stamp(stamp, spec)


top = load(os.path.join(src, "DEPS"))
emit(top, "")
# gclient also evaluates the DEPS of every recursedep (devtools-frontend's
# esbuild, Dawn's Go, ...); with use_relative_paths their keys are repo-relative.
for rd in top.get("recursedeps", []):
    rd, name = (rd, "DEPS") if isinstance(rd, str) else rd
    sub = rd.removeprefix("src/")
    f = os.path.join(src, sub, name)
    if os.path.isfile(f):
        g = load(f)
        emit(g, sub if g.get("use_relative_paths") else "")
NGEOF

prep_identity > "$PREPARED"
cd "$WORK"
}

if [[ "$STAGE" != "build" ]]; then
  prep_tree
fi
if [[ "$STAGE" == "prep" ]]; then
  echo "[browser-build] Prep done: tree ready for concurrent BROWSER_STAGE=build runs."
  exit 0
fi
if [[ "$(cat "$PREPARED" 2>/dev/null)" != "$(prep_identity)" ]]; then
  echo "[browser-build] FATAL: the tree is not prepared for this UC tag + patch series." >&2
  echo "[browser-build] Run BROWSER_STAGE=prep first." >&2
  exit 2
fi

# Stage 6: build ----------------------------------------------------------------
# Drop the previous artifact first: a failed build must not leave an older
# tarball and checksum in $OUT looking like this run's output.
ARTIFACT="$OUT/stealth-chromium-linux-${TARGET_CPU}.tar.gz"
rm -f "$ARTIFACT" "$ARTIFACT.tmp" "$ARTIFACT.sha256"
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
  grep -vE '^(chrome_pgo_phase|enable_remoting|safe_browsing_mode|treat_warnings_as_errors|enable_widevine)=' \
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
# compile the key-system support but ship NO proprietary blob. Nothing fetches
# a CDM either, so Widevine EME still rejects until one is sideloaded.
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
  #   clang_use_chrome_plugins -> ungoogled's flags.gn sets this false for EVERY
  #     build, so it is kept in the merge rather than filtered and is no longer
  #     set here: setting it only under sccache meant a BROWSER_NO_SCCACHE=1
  #     build silently diverged from ungoogled's own config. It maps to
  #     -Xclang -add-plugin (blink-gc / find-bad-constructs
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
  # /work/sccache turns a from-scratch rebuild into minutes. See ../README.md.
  echo "use_clang_modules = false" >> "$OUT_DIR/args.gn"
  echo "use_libcxx_modules = false" >> "$OUT_DIR/args.gn"
fi

DT="$PWD/uc_staging/depot_tools"
GN_BIN="$PWD/buildtools/linux64/gn"

if [[ ! -f /tmp/.browser-build-deps-installed ]]; then
  echo "[browser-build] Running chromium install-build-deps.sh..."
  # arm64 target needs --arm to pull cross libs; x64 keeps --no-arm.
  ARM_FLAG="--no-arm"
  [[ "$TARGET_CPU" == "arm64" ]] && ARM_FLAG="--arm"
  yes | bash build/install-build-deps.sh \
    --no-prompt --no-chromeos-fonts --no-nacl "$ARM_FLAG" 2>&1 | tail -8 || true
  touch /tmp/.browser-build-deps-installed
fi
"$GN_BIN" gen "$OUT_DIR"
# ninja reports only the FIRST missing source input, after minutes of setup, so
# a release that adds several costs a relaunch each. List them all up front.
ninja -C "$OUT_DIR" -t inputs chrome | python3 -c '
import os, sys
out = sys.argv[1]
gone = [f for f in sys.stdin.read().split() if f.startswith("../../") and not os.path.exists(os.path.join(out, f))]
for f in gone[:40]:
    print("[browser-build]   missing input:", f[6:], file=sys.stderr)
sys.exit(1 if gone else 0)
' "$OUT_DIR" || { echo "[browser-build] FATAL: source inputs missing (above); fix prep before building." >&2; exit 2; }
echo "[browser-build] Ninja target: chrome (cpu=$TARGET_CPU)"
# -k 0: a compile error in one stealth patch must not stop the other ~80k
# targets, so one run surfaces every broken patch and warms everything else.
# One retry: at -j48 some upstream edges race their generators (154: devtools'
# esbuild bundle read skills/*.skill.js before generate_skills wrote them). A
# retry clears a race and re-fails a real compile error in seconds.
ninja -C "$OUT_DIR" -j "$(nproc)" -k 0 chrome || {
  echo "[browser-build] ninja failed; retrying once to rule out an ordering race..."
  ninja -C "$OUT_DIR" -j "$(nproc)" -k 0 chrome
}

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
tar -czf "$ARTIFACT.tmp" "${PACKAGE_FILES[@]}"
ARTIFACT_SHA=$(sha256sum "$ARTIFACT.tmp" | cut -d' ' -f1)
mv "$ARTIFACT.tmp" "$ARTIFACT"
echo "[browser-build] Done. Artifact: $ARTIFACT"
ls -lh "$ARTIFACT"
echo "$ARTIFACT_SHA  ${ARTIFACT##*/}" | tee "$ARTIFACT.sha256"
