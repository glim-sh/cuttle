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

# The probe body is validate/smoke.py's CDP_GETTER_PROBE.
code=$(cat <<'JS'
async page => {
  const tab = await page.context().newPage();
  try {
    await tab.goto("about:blank");
    return await tab.evaluate(async () => {
      const probe = () => {
        const reads = (target, key, run) => {
          const saved = Object.getOwnPropertyDescriptor(target, key);
          let n = 0;
          Object.defineProperty(target, key, {configurable: true, get() {
            n++;
            return saved && ("value" in saved ? saved.value : saved.get.call(this));
          }});
          try { run(); } finally {
            if (saved) Object.defineProperty(target, key, saved); else delete target[key];
          }
          return n;
        };
        const columns = [];
        const out = {
          errorName: reads(Error.prototype, "name", () => console.debug(new Error(""))),
          regexpFlag: reads(RegExp.prototype, "global", () => console.debug(/x/g)),
          tableColumn: reads(columns, 0, () => console.table([{a: 1}], columns)),
        };
        if (typeof NodeList !== "undefined") {
          out.nodeListLength = reads(NodeList.prototype, "length",
                                     () => console.debug(document.querySelectorAll("p")));
        }
        return out;
      };
      const src = "self.onmessage = () => self.postMessage((" + probe + ")())";
      const w = new Worker(URL.createObjectURL(new Blob([src], {type: "text/javascript"})));
      const worker = await new Promise((resolve) => {
        w.onmessage = (e) => resolve(e.data);
        w.onerror = (e) => resolve({error: String(e.message)});
        setTimeout(() => resolve({error: "worker timeout"}), 8000);
        w.postMessage(0);
      });
      w.terminate();
      return {page: probe(), worker};
    });
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
