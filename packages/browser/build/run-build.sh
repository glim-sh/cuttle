#!/usr/bin/env bash
# Driver: build the stealth-Chromium binary inside a Docker container on the
# Linux build host (Hetzner CCX), with a persistent /work volume so partial
# progress and the warm cache survive container restarts.
#
# Usage:
#   TARGET_CPU=x64   ./run-build.sh [foreground|background]
#   TARGET_CPU=arm64 ./run-build.sh [foreground|background]
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
CONTAINER_NAME="${BROWSER_BUILD_CONTAINER:-stealth-chromium-build-${TARGET_CPU}}"

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

# Refuse to SIGKILL a build already in flight (re-running the documented
# background invocation would otherwise silently kill a multi-hour run).
if docker ps --filter "name=^${CONTAINER_NAME}$" --format '{{.Names}}' | grep -q .; then
  echo "ERROR: $CONTAINER_NAME is already running. Tail it with:" >&2
  echo "  docker logs -f $CONTAINER_NAME" >&2
  echo "Stop it first, or set FORCE=1 to replace it." >&2
  [[ "${FORCE:-0}" == "1" ]] || exit 1
fi
docker rm -f "$CONTAINER_NAME" 2>/dev/null || true

# The /work volume lives on the mounted Hetzner volume so the ~80 GB checkout,
# fetched toolchains, out/<cpu>, and sccache cache persist across teardown.
CMD=(docker run --name "$CONTAINER_NAME"
  -v "$WORK_MOUNT":/work
  -v "$PKG/patches":/patches:ro
  -v "$HERE/build-linux.sh":/usr/local/bin/build-linux.sh:ro
  -v "$PKG/validate":/work/packages/browser/validate:ro
  -v "$OUT_DIR":/out
  -e "BROWSER_WORK_DIR=/work"
  -e "BROWSER_UC_TAG=${UC_TAG}"
  -e "TARGET_CPU=${TARGET_CPU}"
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
