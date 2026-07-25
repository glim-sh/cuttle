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
  # `server delete` is an API-level plug-pull on a live ext4 mount. Refuse while a
  # build is in flight (the documented flow runs it in background), and flush the
  # cache volume first so the warm checkout is not left dirty.
  if [[ "${FORCE:-0}" != "1" ]]; then
    # Distinguish "confirmed idle" from "could not determine". ssh failing (exit
    # 255: unreachable, key problem, still booting) is NOT evidence that no build
    # is running, and deleting on that assumption plug-pulls a live ext4 mount.
    IP="$(hcloud server ip "$SERVER_NAME" 2>/dev/null || true)"
    if [[ -z "$IP" ]]; then
      echo "ERROR: could not resolve an IP for $SERVER_NAME, so a running build" >&2
      echo "cannot be ruled out. Re-run with FORCE=1 to delete anyway." >&2
      exit 1
    fi
    running="$(ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 "root@$IP" \
      'docker ps --filter name=stealth-chromium-build --format "{{.Names}}"' 2>/dev/null)"
    rc=$?
    if (( rc != 0 )); then
      echo "ERROR: could not reach $SERVER_NAME (ssh exit $rc) to check for a" >&2
      echo "running build. Re-run with FORCE=1 to delete anyway." >&2
      exit 1
    fi
    if [[ -n "$running" ]]; then
      echo "ERROR: a build container is still running on $SERVER_NAME: $running" >&2
      echo "Wait for it, or re-run with FORCE=1 to delete anyway." >&2
      exit 1
    fi
    ssh -o ConnectTimeout=5 "root@$IP" 'sync; umount /work 2>/dev/null || true' 2>/dev/null || true
  fi
  echo "[teardown] Deleting server $SERVER_NAME (volume $VOLUME_NAME kept)..."
  hcloud server delete "$SERVER_NAME"
else
  echo "[teardown] Server $SERVER_NAME already gone."
fi
echo "[teardown] Volume $VOLUME_NAME retained. Re-provision to resume warm."
