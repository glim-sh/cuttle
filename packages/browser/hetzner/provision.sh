#!/usr/bin/env bash
# Provision the Hetzner build box + persistent cache volume for the stealth-
# Chromium pipeline. Idempotent: re-running against an existing volume reuses
# the warm cache (cloud-init only formats an unformatted volume).
#
# Auth: reads HCLOUD_TOKEN from the environment. This script NEVER writes the
# token anywhere. Export it before running (e.g. from ~/.zshenv):
#   export HCLOUD_TOKEN=...   # or: hcloud context use <ctx>
#
# Usage:
#   SERVER_TYPE=ccx63 ./provision.sh      # Phase 1 cold build (fast, 48 vCPU)
#   ./provision.sh                        # later incrementals (ccx53 default)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

SERVER_NAME="${SERVER_NAME:-cuttle-builder}"
VOLUME_NAME="${VOLUME_NAME:-cuttle-build-cache}"
VOLUME_SIZE="${VOLUME_SIZE:-500}"          # GB; holds src + out/{x64,arm64} + sccache
SERVER_TYPE="${SERVER_TYPE:-ccx53}"        # dedicated vCPU AMD EPYC, hourly
LOCATION="${LOCATION:-nbg1}"
IMAGE="${IMAGE:-ubuntu-24.04}"
SSH_KEY="${SSH_KEY:-}"                      # hcloud ssh-key name; auto-detected if empty

if [[ -z "${HCLOUD_TOKEN:-}" ]] && ! hcloud context active >/dev/null 2>&1; then
  echo "ERROR: set HCLOUD_TOKEN or select an hcloud context first." >&2
  exit 1
fi

# Pick an ssh key: explicit SSH_KEY, else the first one Hetzner knows about.
if [[ -z "$SSH_KEY" ]]; then
  SSH_KEY="$(hcloud ssh-key list -o noheader -o columns=name | head -1 || true)"
  [[ -z "$SSH_KEY" ]] && { echo "ERROR: no hcloud ssh-key; create one with 'hcloud ssh-key create'." >&2; exit 1; }
fi
echo "[provision] ssh-key=$SSH_KEY server-type=$SERVER_TYPE location=$LOCATION"

# Volume (persistent warm cache) - create only if absent.
if ! hcloud volume describe "$VOLUME_NAME" >/dev/null 2>&1; then
  echo "[provision] Creating ${VOLUME_SIZE}GB volume $VOLUME_NAME in $LOCATION..."
  hcloud volume create --name "$VOLUME_NAME" --size "$VOLUME_SIZE" --location "$LOCATION" --format ext4
else
  echo "[provision] Reusing existing volume $VOLUME_NAME (warm cache preserved)."
fi

# Server - create only if absent.
if ! hcloud server describe "$SERVER_NAME" >/dev/null 2>&1; then
  echo "[provision] Creating server $SERVER_NAME ($SERVER_TYPE)..."
  hcloud server create \
    --name "$SERVER_NAME" \
    --type "$SERVER_TYPE" \
    --image "$IMAGE" \
    --location "$LOCATION" \
    --ssh-key "$SSH_KEY" \
    --volume "$VOLUME_NAME" \
    --user-data-from-file "$HERE/cloud-init.yaml"
else
  echo "[provision] Server $SERVER_NAME already exists."
fi

IP="$(hcloud server ip "$SERVER_NAME")"
echo "[provision] Server IP: $IP"
echo "[provision] Waiting for SSH..."
for _ in $(seq 1 60); do
  if ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 "root@$IP" true 2>/dev/null; then
    break
  fi
  sleep 5
done
echo "[provision] Ready. Connect with:"
echo "  ssh root@$IP"
echo "[provision] Then, on the box:"
echo "  git clone <this-repo> /work/repo && cd /work/repo"
echo "  TARGET_CPU=x64   packages/browser/build/run-build.sh background"
echo "[provision] Tear down compute (keeps the volume) with: hetzner/teardown.sh"
