#!/usr/bin/env bash
# Driver: build the stealth-Chromium binary inside a Docker container on the
# Linux build host (Hetzner CCX), with a persistent /work volume so partial
# progress and the warm cache survive container restarts.
#
# Usage:
#   TARGET_CPU=x64   ./run-build.sh [foreground|background]
#   TARGET_CPU=arm64 ./run-build.sh [foreground|background]
#
# Both targets at once: prep the shared tree once, then build them concurrently.
#   BROWSER_STAGE=prep ./run-build.sh foreground
#   BROWSER_STAGE=build TARGET_CPU=x64   ./run-build.sh background
#   BROWSER_STAGE=build TARGET_CPU=arm64 ./run-build.sh background
#
# Expects this repo checked out on the host with the persistent volume mounted
# at /work (see hetzner/cloud-init.yaml). Reads versions.env for UC_TAG.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PKG="$(cd "$HERE/.." && pwd)"          # packages/browser

# shellcheck disable=SC1091
source "$PKG/versions.env"

WORK_MOUNT="${BROWSER_WORK_MOUNT:-/work}"
OUT_DIR="${BROWSER_OUT_DIR:-/work/dist}"
IMAGE="${BROWSER_BUILD_IMAGE:-stealth-chromium-build:latest}"
TARGET_CPU="${TARGET_CPU:-x64}"
MODE="${1:-foreground}"
CPU_COUNT="$(nproc 2>/dev/null || echo 16)"
STAGE="${BROWSER_STAGE:-all}"
# prep writes the shared tree and is not per-target, so it gets its own name.
CONTAINER_SUFFIX="$TARGET_CPU"; [[ "$STAGE" == "prep" ]] && CONTAINER_SUFFIX="prep"
CONTAINER_NAME="${BROWSER_BUILD_CONTAINER:-stealth-chromium-build-${CONTAINER_SUFFIX}}"

# $WORK_MOUNT must be the mounted cache volume. cloud-init exits 0 without
# mounting when the device is not visible yet (attach/udev race), and an ~80GB
# checkout onto the ephemeral root disk is then destroyed by teardown.sh. This
# has to be checked HERE: inside the container /work is a bind mount and so is
# always a mountpoint, whether or not the host volume is mounted.
dev_of() { stat -c %d "$1" 2>/dev/null || stat -f %d "$1"; }
if [[ "${BROWSER_ALLOW_UNMOUNTED_WORK:-0}" != "1" ]] \
   && [[ "$(dev_of "$WORK_MOUNT")" == "$(dev_of /)" ]]; then
  echo "ERROR: $WORK_MOUNT is on the same device as / - the cache volume is not mounted." >&2
  echo "Building would fill the root disk and be lost on teardown. Mount it, or set" >&2
  echo "BROWSER_ALLOW_UNMOUNTED_WORK=1 to build on local disk deliberately." >&2
  exit 2
fi

mkdir -p "$OUT_DIR"

echo "[run-build] Building image $IMAGE (host arch: $(uname -m))..."
docker build -t "$IMAGE" -f "$HERE/Dockerfile.linux" "$HERE"

# install-build-deps.sh is an apt install that every container would otherwise
# repeat (1-2 min per launch, paid again on every rebuild). It comes from the
# Chromium tree, so bake it once per script content + base image and reuse it
# (keyed on the script itself: a re-prep can change it without a version bump).
DEPS_SCRIPT="$WORK_MOUNT/build/src/build/install-build-deps.sh"
if [[ -f "$DEPS_SCRIPT" ]]; then
  DEPS_IMAGE="${IMAGE%:*}:deps-${CHROMIUM_VERSION}-$(sha256sum "$DEPS_SCRIPT" | cut -c1-12)-$(docker image inspect -f '{{.Id}}' "$IMAGE" | cut -c8-19)"
  if ! docker image inspect "$DEPS_IMAGE" >/dev/null 2>&1; then
    echo "[run-build] Baking $DEPS_IMAGE (install-build-deps, once per script)..."
    DEPS_CONTAINER="stealth-chromium-deps-$$"
    # --arm is a superset: the arm64 cross libs plus everything x64 needs.
    docker run --name "$DEPS_CONTAINER" -v "$WORK_MOUNT/build/src":/src:ro -w /src "$IMAGE" \
      bash -c 'yes | bash build/install-build-deps.sh --no-prompt --no-chromeos-fonts --no-nacl --arm \
               && touch /tmp/.browser-build-deps-installed' | tail -3
    docker commit "$DEPS_CONTAINER" "$DEPS_IMAGE" >/dev/null
    docker rm "$DEPS_CONTAINER" >/dev/null
  fi
  IMAGE="$DEPS_IMAGE"
fi

# Refuse to SIGKILL a build already in flight (re-running the documented
# background invocation would otherwise silently kill a multi-hour run).
if docker ps --filter "name=^${CONTAINER_NAME}$" --format '{{.Names}}' | grep -q .; then
  echo "ERROR: $CONTAINER_NAME is already running. Tail it with:" >&2
  echo "  docker logs -f $CONTAINER_NAME" >&2
  echo "Stop it first, or set FORCE=1 to replace it." >&2
  [[ "${FORCE:-0}" == "1" ]] || exit 1
fi
# prep rewrites the shared tree that a running per-target build compiles from.
if [[ "$STAGE" == "prep" ]]; then
  BUSY="$(docker ps --filter 'name=^stealth-chromium-build-' --format '{{.Names}}' | grep -vx "$CONTAINER_NAME" || true)"
  if [[ -n "$BUSY" ]]; then
    echo "ERROR: prep would rewrite the tree under running build(s): $BUSY" >&2
    echo "Wait for them to finish, or stop them first." >&2
    exit 1
  fi
fi
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

# The /work volume lives on the mounted Hetzner volume so the ~80 GB checkout,
# fetched toolchains, out/<cpu>, and sccache cache persist across teardown.
CMD=(docker run --name "$CONTAINER_NAME"
  -v "$WORK_MOUNT":/work
  -v "$PKG/patches":/patches:ro
  -v "$HERE/build-linux.sh":/usr/local/bin/build-linux.sh:ro
  -v "$PKG/validate":/work/packages/browser/validate:ro
  # smoke.py reads the pinned version from here rather than hardcoding it, so
  # versions.env has to be mounted beside validate/ or the gate cannot start.
  -v "$PKG/versions.env":/work/packages/browser/versions.env:ro
  # ...and the golden, because the gate has to launch Chrome with the daemon's
  # own baseChromeArgs. Composing its own list is how it ended up gating a
  # browser we never ship.
  -v "$PKG/../cuttle/internal/fingerprint/testdata/golden.json":/work/golden.json:ro
  -v "$OUT_DIR":/out
  -e "BROWSER_WORK_DIR=/work"
  -e "GOLDEN_JSON=/work/golden.json"
  -e "BROWSER_UC_TAG=${UC_TAG}"
  -e "TARGET_CPU=${TARGET_CPU}"
  -e "BROWSER_STAGE=${STAGE}"
  -e "SCCACHE_DIR=/work/sccache"
  # sccache only evicts at its own cap, so this must stay well below the free
  # space on $WORK_MOUNT or it fills the disk instead of recycling.
  -e "SCCACHE_CACHE_SIZE=${SCCACHE_CACHE_SIZE:-40G}"
  --cpus="$CPU_COUNT"
)
[[ "$MODE" == "background" ]] && CMD+=(-d)
CMD+=("$IMAGE" bash /usr/local/bin/build-linux.sh)

if [[ "$MODE" == "background" ]]; then
  echo "[run-build] Starting in background. Tail with: docker logs -f $CONTAINER_NAME"
  exec "${CMD[@]}" >/dev/null
else
  exec "${CMD[@]}"
fi
