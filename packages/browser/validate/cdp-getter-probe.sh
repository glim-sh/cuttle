#!/usr/bin/env bash
# Release gate for patch 0056 (issue #50): the console-preview getter probe run
# through the playwright-cli bundled in the image, so the driver's own
# Runtime.enable is live - on the page and, by its auto-attach, in a worker.
# smoke.py and detect.py enable Runtime by hand; this proves the same against
# the driver we ship. It opens its own tab and closes it, leaving the session's
# tabs alone.
#
# Every count must equal a browser with no debugger attached. Unpatched, each
# Runtime-enabled session adds one read to every count.
#
# Usage: cdp-getter-probe.sh [cuttle flags, e.g. --name <instance>]
set -euo pipefail

want='{"page":{"errorName":1,"regexpFlag":1,"tableColumn":1,"nodeListLength":0},"worker":{"errorName":1,"regexpFlag":1,"tableColumn":1}}'

# The probe body is smoke.py's CDP_GETTER_PROBE, cut out by its delimiters.
probe=$(sed -n '/^CDP_GETTER_PROBE = r"""/,/^}"""$/p' "$(dirname "$0")/smoke.py" \
  | sed -e '1s/^CDP_GETTER_PROBE = r"""//' -e '$s/"""$//')
[[ -n "$probe" ]] || { echo "CDP_GETTER_PROBE not found in smoke.py" >&2; exit 2; }
code=$(cat <<JS
async page => {
  const tab = await page.context().newPage();
  try {
    await tab.goto("about:blank");
    return await tab.evaluate($probe);
  } finally {
    await tab.close();
  }
}
JS
)

out=$(cuttle "$@" pw run-code "$code")
got=$(printf '%s\n' "$out" | sed -n '/^### Result$/{n;p;q;}')
echo "want $want"
echo "got  $got"
if [[ "$got" != "$want" ]]; then
  echo "FAIL: page getters fire during console previews while the driver is attached" >&2
  exit 1
fi
echo "PASS"
