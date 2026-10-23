#!/usr/bin/env bash
# Release gate for `cuttle serve` itself: the image's daemon in pool and session
# mode, each on Xvfb and on KasmVNC (CUTTLE_VNC=1), measured from inside its own
# container by daemon-gate.py. Run each persona on its own arch: windows on an
# amd64 host, macos on Apple Silicon (OrbStack or Docker Desktop).
#
# Usage: PYLIBS=<dir> daemon-gate.sh <image> <windows|macos>
#   PYLIBS           the unpacked websocket-client wheel; the image has no pip (required)
#   GATE_MODES       subset of "pool pool-vnc session session-vnc" (default: all four)
#   GATE_NAME        container name prefix (default: daemon-gate)
#   GATE_OUT         log dir (default: ./daemon-gate-out)
# Exit status: the number of modes with a failed assertion.
set -uo pipefail

usage="usage: PYLIBS=<dir> $0 <image> <windows|macos>"
image=${1:?$usage}
persona=${2:?$usage}
case $persona in
  windows) platform=linux/amd64 ;;
  macos) platform=linux/arm64 ;;
  *) echo "$usage" >&2; exit 2 ;;
esac
pylibs=$(cd "${PYLIBS:?$usage}" && pwd) || exit 2
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
out=${GATE_OUT:-$PWD/daemon-gate-out}
mkdir -p "$out" && out=$(cd "$out" && pwd)
b=/work/packages/browser

modes=${GATE_MODES:-pool pool-vnc session session-vnc}
for mode in $modes; do
  case $mode in
    pool | pool-vnc | session | session-vnc) ;;
    *) echo "unknown mode $mode" >&2; exit 2 ;;
  esac
done

name=
trap '[[ -z $name ]] || docker rm -f "$name" >/dev/null 2>&1' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

failed=0
for mode in $modes; do
  name=${GATE_NAME:-daemon-gate}-$mode
  vnc=0 cmd=() args=()
  case $mode in
    pool | pool-vnc) cmd=(cuttle serve --headless=false --mode=pool --idle-timeout=600 --fingerprint=daemon-gate)
      args=(--json /tmp/detect.json) ;;
    session | session-vnc) ;; # the image's default CMD: session mode, as `cuttle up` runs it
  esac
  [[ $mode == *-vnc ]] && vnc=1 args+=(--vnc)
  echo "=== daemon-gate $persona/$mode ($image)"
  docker rm -f "$name" >/dev/null 2>&1
  rm -f "$out/$persona-$mode.json"
  # golden.json and prefs.json only satisfy detect.py's import-time guards: the daemon launches Chrome, not detect.
  if ! docker run -d --name "$name" --platform "$platform" --shm-size=2g -e CUTTLE_VNC=$vnc \
    -v "$repo/packages/browser/benches:$b/benches:ro" -v "$here:$b/validate:ro" \
    -v "$repo/packages/cuttle/internal/fingerprint/testdata/golden.json:/golden.json:ro" \
    -v "$repo/packages/cuttle/internal/serve/testdata/fresh-profile-prefs.json:/prefs.json:ro" \
    -v "$pylibs:/pylibs:ro" "$image" ${cmd[@]+"${cmd[@]}"} >/dev/null; then
    failed=$((failed + 1))
    continue
  fi
  for _ in $(seq 90); do
    docker exec "$name" curl -fs http://127.0.0.1:9222/readyz >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec -e PYTHONPATH=/pylibs -e PYTHONDONTWRITEBYTECODE=1 \
    -e BROWSER_BINARY_PATH=/opt/browser/chrome -e GOLDEN_JSON=/golden.json -e PROFILE_PREFS_JSON=/prefs.json \
    "$name" python3 "$b/validate/daemon-gate.py" "$persona" "${mode%-vnc}" ${args[@]+"${args[@]}"} 2>&1 | tee "$out/$persona-$mode.log" &
  # A foreground pipeline would defer the INT/TERM trap until the run ends; wait returns at once.
  wait $!
  rc=$?
  [[ $mode == pool* ]] && docker cp "$name:/tmp/detect.json" "$out/$persona-$mode.json" >/dev/null 2>&1
  docker logs "$name" >"$out/$persona-$mode.serve.log" 2>&1
  docker rm -f "$name" >/dev/null
  echo "[daemon-gate] $persona/$mode exit=$rc"
  [[ $rc == 0 ]] || failed=$((failed + 1))
done
echo "[daemon-gate] $persona: $failed mode(s) failed, logs in $out"
exit "$failed"
