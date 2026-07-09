#!/usr/bin/env python3
"""cuttle smoke harness - neutral, self-contained.

Drives a running cuttle over CDP and introspects each seed's browser directly -
no third-party sites, no network targets, no local server. It checks:

  1. per-seed fingerprint isolation - each fingerprint seed gets its own coherent
     identity, so an in-page canvas readback differs across seeds.
  2. stealth coherence - navigator.webdriver is falsy and the UA/platform agree
     (a Windows UA must not pair with a non-Windows platform).
  3. connection stability under cold-cycle load - fresh seeds are launched in a
     loop; every cycle must connect and probe without error.

Run:  uv run python test/harness.py   (from the repo root; see test/README.md)
Green = distinct per-seed canvas, coherent stealth signals, no failures.

This is a fast, client-agnostic local smoke. It does NOT reproduce the
playwright-core service_worker crash (only a playwright client can), nor does it
clear real challenges - both are validated separately against a real amd64
deployment. See ../docs/UPGRADE.md.
"""

import asyncio
import json
import os
import time
import urllib.request
from urllib.parse import urlencode, urlsplit, urlunsplit

import websockets

CUTTLE_URL = os.environ.get("CUTTLE_URL", "http://127.0.0.1:9222")
COLD_CYCLES = int(os.environ.get("COLD_CYCLES", "3"))
RUN_ID = format(int(time.time()), "x")

CONNECT_TIMEOUT = 90

# One self-contained expression: build a canvas (farbling is fingerprint-seeded,
# so it differs per seed) and read the stealth signals, returned as JSON.
PROBE_JS = r"""
(() => {
  let canvas = "missing";
  try {
    const c = document.createElement("canvas");
    c.width = 200; c.height = 40;
    const ctx = c.getContext("2d");
    ctx.textBaseline = "top";
    ctx.font = "16px Arial";
    ctx.fillStyle = "#f60"; ctx.fillRect(0, 0, 200, 40);
    ctx.fillStyle = "#069"; ctx.fillText("cuttle-smoke", 2, 2);
    canvas = c.toDataURL();
  } catch (e) { canvas = "canvas-error:" + e.message; }
  return JSON.stringify({
    webdriver: navigator.webdriver,
    ua: navigator.userAgent,
    platform: navigator.platform,
    canvas,
  });
})()
"""


def browser_ws_for_seed(seed):
    """Ask cuttle for the seed's browser CDP WebSocket (this launches the seed)."""
    parts = urlsplit(CUTTLE_URL)
    url = urlunsplit(
        (parts.scheme, parts.netloc, "/json/version", urlencode({"fingerprint": seed}), "")
    )
    with urllib.request.urlopen(url, timeout=30) as r:
        return json.load(r)["webSocketDebuggerUrl"]


class CDP:
    """Minimal CDP client: send a command, return its result (skipping events)."""

    def __init__(self, ws):
        self.ws = ws
        self._id = 0

    async def send(self, method, params=None, session_id=None):
        self._id += 1
        mid = self._id
        msg = {"id": mid, "method": method, "params": params or {}}
        if session_id:
            msg["sessionId"] = session_id
        await self.ws.send(json.dumps(msg))
        while True:
            resp = json.loads(await self.ws.recv())
            if resp.get("id") == mid:
                if "error" in resp:
                    raise RuntimeError(f"{method}: {resp['error']}")
                return resp.get("result", {})


async def probe_seed(seed):
    ws_url = browser_ws_for_seed(seed)
    async with websockets.connect(ws_url, max_size=None, open_timeout=CONNECT_TIMEOUT) as ws:
        cdp = CDP(ws)
        target = await cdp.send("Target.createTarget", {"url": "about:blank"})
        target_id = target["targetId"]
        attached = await cdp.send("Target.attachToTarget", {"targetId": target_id, "flatten": True})
        session_id = attached["sessionId"]
        result = await cdp.send(
            "Runtime.evaluate",
            {"expression": PROBE_JS, "returnByValue": True, "awaitPromise": True},
            session_id=session_id,
        )
        await cdp.send("Target.closeTarget", {"targetId": target_id})
        return json.loads(result["result"]["value"])


results = []  # (name, status, detail)
canvas_by_seed = {}


async def cold_cycle(cycle):
    seed = f"smoke-{RUN_ID}-{cycle}"
    name = f"cold-cycle-{cycle}"
    try:
        info = await asyncio.wait_for(probe_seed(seed), timeout=CONNECT_TIMEOUT + 30)
    except Exception as exc:  # a connection/probe failure IS the stability signal
        results.append((name, "fail", f"probe failed: {exc}"))
        return

    ua, platform = info.get("ua", ""), info.get("platform", "")
    canvas = info.get("canvas", "missing")
    canvas_by_seed[seed] = canvas

    problems = []
    if info.get("webdriver"):
        problems.append(f"webdriver={info['webdriver']}")
    if ("windows" in ua.lower()) != platform.lower().startswith("win"):
        problems.append(f"incoherent ua/platform (ua={ua[:40]!r} platform={platform!r})")
    if not canvas.startswith("data:image"):
        problems.append(f"canvas={canvas[:24]}")

    if problems:
        results.append((name, "fail", "; ".join(problems)))
    else:
        results.append(
            (
                name,
                "pass",
                f"webdriver={info.get('webdriver')} platform={platform} canvas=ok seed={seed}",
            )
        )


def check_canvas_isolation():
    values = [v for v in canvas_by_seed.values() if v.startswith("data:image")]
    if len(values) < 2:
        results.append(
            ("canvas-isolation", "fail", f"need >=2 canvas readbacks, got {len(values)}")
        )
        return
    distinct = len(set(values))
    if distinct < 2:
        results.append(
            (
                "canvas-isolation",
                "fail",
                f"all {len(values)} seeds produced an identical canvas (no per-seed farbling)",
            )
        )
    else:
        results.append(
            (
                "canvas-isolation",
                "pass",
                f"{distinct} distinct canvas fingerprints across {len(values)} seeds",
            )
        )


async def main():
    print("cuttle smoke harness")
    print(f"  CUTTLE_URL  = {CUTTLE_URL}")
    print(f"  cold cycles = {COLD_CYCLES}")

    print("\n== stealth coherence + connection stability (cold cycles) ==")
    for i in range(1, COLD_CYCLES + 1):
        await cold_cycle(i)

    print("\n== per-seed fingerprint isolation ==")
    check_canvas_isolation()

    for name, status, detail in results:
        print(f"  [{status.upper()}] {name} - {detail}")

    passed = sum(1 for _, s, _ in results if s == "pass")
    print("\n================ SUMMARY ================")
    print(f"  cases  {passed}/{len(results)} pass")
    print("========================================")
    green = all(s == "pass" for _, s, _ in results)
    print(
        "\nGREEN: per-seed isolation, coherent stealth, no failures.\n"
        if green
        else "\nNOT GREEN: see failures above.\n"
    )
    return 0 if green else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
