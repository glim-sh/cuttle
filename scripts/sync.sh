#!/usr/bin/env bash
# Re-sync the vendored cloakbrowser subset for review. This does NOT overwrite
# the vendored files (they carry deliberate trims/stubs - see docs/UPSTREAM.md).
# It fetches the pinned upstream files into a temp dir and prints a diff against
# ours, so an upstream bump is a reviewable change you re-apply by hand.
set -euo pipefail

REF="${CUTTLE_UPSTREAM_REF:-v0.4.9}"
REPO="${CUTTLE_UPSTREAM_REPO:-https://github.com/CloakHQ/cloakbrowser}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Fetching $REPO @ $REF ..."
git clone --quiet --depth 1 --branch "$REF" "$REPO" "$TMP/upstream"

for f in config.py geoip.py browser.py download.py; do
  echo ""
  echo "===== vendor/cloakbrowser/$f  vs  upstream cloakbrowser/$f ====="
  diff -u "$TMP/upstream/cloakbrowser/$f" "$HERE/vendor/cloakbrowser/$f" || true
done

echo ""
echo "===== bin/cuttleserve  vs  upstream bin/cloakserve ====="
diff -u "$TMP/upstream/bin/cloakserve" "$HERE/bin/cuttleserve" || true

echo ""
echo "Review the diffs above, re-apply cuttle's trims/patches as needed, then"
echo "update the pinned ref in docs/UPSTREAM.md."
