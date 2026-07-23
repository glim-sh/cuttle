#!/usr/bin/env bash
# Stop paying for compute: delete the build server but KEEP the persistent
# cache volume so the next build resumes warm. (Powering off a Hetzner server
# still bills; deleting it is the real "stop".)
#
# Auth: reads HCLOUD_TOKEN from the environment (never written anywhere).
#
# Usage: ./teardown.sh
set -euo pipefail

SERVER_NAME="${SERVER_NAME:-cuttle-builder}"
VOLUME_NAME="${VOLUME_NAME:-cuttle-build-cache}"

if [[ -z "${HCLOUD_TOKEN:-}" ]] && ! hcloud context active >/dev/null 2>&1; then
  echo "ERROR: set HCLOUD_TOKEN or select an hcloud context first." >&2
  exit 1
fi

if hcloud server describe "$SERVER_NAME" >/dev/null 2>&1; then
  echo "[teardown] Deleting server $SERVER_NAME (volume $VOLUME_NAME kept)..."
  hcloud server delete "$SERVER_NAME"
else
  echo "[teardown] Server $SERVER_NAME already gone."
fi
echo "[teardown] Volume $VOLUME_NAME retained. Re-provision to resume warm."
