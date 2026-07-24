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
BROWSER_TARGET="${BROWSER_TARGET:-chrome}"
MODE="${1:-foreground}"
CPU_COUNT="$(nproc 2>/dev/null || echo 16)"
CONTAINER_NAME="${BROWSER_BUILD_CONTAINER:-stealth-chromium-build-${TARGET_CPU}}"

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
  -e "BROWSER_TARGET=${BROWSER_TARGET}"
  -e "BROWSER_UC_TAG=${UC_TAG}"
  -e "TARGET_CPU=${TARGET_CPU}"
  -e "SCCACHE_DIR=/work/sccache"
  -e "SCCACHE_CACHE_SIZE=${SCCACHE_CACHE_SIZE:-150G}"
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
