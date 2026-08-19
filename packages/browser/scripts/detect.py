#!/usr/bin/env python3
# SPDX: MIT
"""Manual detector harness: run a built binary against external detectors.

Deliberately NOT a gate. These pages are third-party and change without notice,
and CreepJS deleted its trust score upstream in 2025, so there is no stable
number to assert. This exists to make step 7 of the release workflow repeatable
and attributable: one command per persona, output pasted into the release notes
beside the sha it describes.

The flag set is READ from internal/fingerprint/testdata/golden.json rather than
restated here. Hand-copied flag sets were the cause of two false findings during
the 151 rebase - a probe missing --enable-features=WebBluetooth "discovered" that
navigator.bluetooth was absent, and one missing --fingerprint-device-memory
"discovered" a memory incoherence. Both were the harness, not the browser. The
golden is what production emits, so deriving from it makes that class impossible.

Only abrahamjuliot.github.io is the genuine CreepJS; the creepjs.org/.com style
domains are flagged upstream as malicious mirrors.

What it runs, and why each one earns its place:

  persona basics      what production actually reports, from a secure origin
  worker coherence    worker realm vs main thread - the classic spoof-only-the-
                      main-thread miss, which no external page reports cleanly
  CreepJS             persona coherence (platformEstimate reads the font pack)
                      and whether our OWN spoofs are named as lies
  are_you_a_bot       the only public source of isAutomatedWithCDP and
                      hasCDPMouseLeak - our largest structural exposure, since
                      we dispatch input over raw CDP
  botstop             an independent verdict from a live production engine;
                      no signal breakdown by design, so a smoke alarm not a
                      diagnostic
  tcp/ip classify     the OS guess from our TCP SYN, which is structurally the
                      container's Linux and contradicts both personas

Every external section is isolated: a third-party page being down, slow or
restructured degrades one line and never fails the run. None of this is a gate.

Usage: BROWSER_BINARY_PATH=... detect.py <windows|macos>   (needs DISPLAY)
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

try:
    import websocket  # type: ignore
except ImportError:
    print("ERROR: pip install websocket-client", file=sys.stderr)
    sys.exit(2)

BINARY = os.environ.get("BROWSER_BINARY_PATH")
if not BINARY or not Path(BINARY).exists():
    sys.exit(f"ERROR: BROWSER_BINARY_PATH not set or missing: {BINARY!r}")
if not os.environ.get("DISPLAY"):
    sys.exit("ERROR: no DISPLAY. WebGL reads back as two empty strings without one, "
             "which every detector scores as a broken GPU spoof.")

PERSONA = (sys.argv[1] if len(sys.argv) > 1 else "windows").lower()
ARCH = {"windows": "amd64", "macos": "arm64"}[PERSONA]
GOLDEN = Path(os.environ.get(
    "GOLDEN_JSON",
    Path(__file__).resolve().parents[3] / "internal/fingerprint/testdata/golden.json"))
PORT = int(os.environ.get("DETECT_CDP_PORT", "9971"))


def production_args() -> list[str]:
    """The composed argv the daemon actually launches with, for this persona.

    ForkParityArgs is only one contributor. getDefaultStealthArgs supplies the
    --fingerprint seed (without which the seed-derived noise never engages, so a
    probe silently measures a different browser), and AppleSiliconArgs pins the
    Mac machine on arm64 only - which is why the Windows persona ships no
    hardware-concurrency or device-memory at all. Deduplicated by flag key with
    later contributors winning, mirroring BuildArgs.
    """
    g = json.loads(GOLDEN.read_text())
    parts: list[list[str]] = []
    for e in g["default_stealth_args"]:
        if e.get("arch") == ARCH:
            parts.append(e["output"])
            break
    for e in g["fork_parity_args"]:
        if e.get("arch") == ARCH and not e.get("locale") and not e.get("proxy"):
            parts.append(e["output"])
            break
    if ARCH == "arm64":
        # A representative machine from the pool; production picks by seed.
        for e in g["apple_silicon_args"]:
            if e.get("arch") == "arm64":
                parts.append(e["output"])
                break
    if len(parts) < 2:
        sys.exit(f"ERROR: could not compose {ARCH} argv from {GOLDEN}")
    merged: dict[str, str] = {}
    for group in parts:
        for arg in group:
            merged[arg.split("=", 1)[0]] = arg
    return list(merged.values())


class Session:
    def __init__(self, extra: list[str]) -> None:
        self.profile = tempfile.mkdtemp()
        self.proc = subprocess.Popen(
            [BINARY, f"--remote-debugging-port={PORT}", f"--user-data-dir={self.profile}",
             "--no-sandbox", "--no-first-run", "--no-default-browser-check",
             "--remote-allow-origins=*", *production_args(), *extra, "about:blank"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        targets = None
        for _ in range(120):
            try:
                targets = json.load(urllib.request.urlopen(
                    f"http://127.0.0.1:{PORT}/json/list", timeout=1))
                break
            except Exception:
                time.sleep(0.5)
        if not targets:
            sys.exit("ERROR: browser never exposed a CDP target")
        page = next(t for t in targets if t["type"] == "page")
        # Generous: a detector page blocks the renderer for tens of seconds, and a
        # short timeout kills the probe mid-measurement rather than waiting it out.
        self.ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=180)
        self._id = 0

    def cmd(self, method: str, params: dict) -> dict:
        self._id += 1
        self.ws.send(json.dumps({"id": self._id, "method": method, "params": params}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get("id") == self._id:
                return msg

    def eval(self, expr: str, await_promise: bool = False):
        r = self.cmd("Runtime.evaluate", {"expression": expr, "returnByValue": True,
                                          "awaitPromise": await_promise})
        return r.get("result", {}).get("result", {}).get("value")

    def reconnect(self) -> None:
        """Re-attach after a socket timeout. The browser is still alive."""
        try:
            self.ws.close()
        except Exception:
            pass
        for _ in range(60):
            try:
                targets = json.load(urllib.request.urlopen(
                    f"http://127.0.0.1:{PORT}/json/list", timeout=2))
                page = next(t for t in targets if t["type"] == "page")
                self.ws = websocket.create_connection(
                    page["webSocketDebuggerUrl"], timeout=180)
                self._id = 0
                return
            except Exception:
                time.sleep(1)
        raise RuntimeError("could not re-attach to the browser")

    def close(self) -> None:
        try:
            self.ws.close()
        finally:
            self.proc.terminate()
            self.proc.wait(timeout=15)


def persona_basics(s: Session) -> None:
    """What production actually reports, as opposed to what the smoke passes in.

    The smoke supplies --fingerprint-hardware-concurrency and, on macOS,
    --fingerprint-device-memory. ForkParityArgs supplies neither on Windows, so
    the gate has never observed the shipped values for those two - the binary's
    own seed-derived pools decide them. Print them.
    """
    print(f"=== persona basics / {PERSONA} (production flags only) ===")
    # Secure context required: on about:blank the device APIs and userAgentData
    # all read as absent, which is indistinguishable from "the build lacks them".
    s.cmd("Page.navigate", {"url": f"http://127.0.0.1:{PORT}/json/version"})
    time.sleep(2)
    d = json.loads(s.eval(
        "JSON.stringify({ua: navigator.userAgent, platform: navigator.platform,"
        " hc: navigator.hardwareConcurrency, mem: navigator.deviceMemory,"
        " touch: navigator.maxTouchPoints, langs: navigator.languages,"
        " share: typeof navigator.share, bluetooth: typeof navigator.bluetooth,"
        " usb: typeof navigator.usb,"
        " heapLimitGB: (performance.memory ? +(performance.memory.jsHeapSizeLimit/1073741824).toFixed(2) : null)})"))
    for k, v in d.items():
        print(f"  {k:12} {json.dumps(v)}")
    if d.get("mem") and d.get("heapLimitGB") and d["heapLimitGB"] > d["mem"]:
        print(f"  !! deviceMemory {d['mem']}GB < JS heap limit {d['heapLimitGB']}GB"
              " - detectors flag this pair as incoherent")
    record("no HeadlessChrome token", "HeadlessChrome" not in (d.get("ua") or ""))
    record("deviceMemory exposed", d.get("mem") is not None,
           "real Chrome always exposes it" if d.get("mem") is None else f"{d.get('mem')}GB")
    record("device APIs present", d.get("bluetooth") == "object" and d.get("usb") == "object",
           f"bluetooth={d.get('bluetooth')} usb={d.get('usb')}")


def creepjs(s: Session) -> None:
    print(f"=== CreepJS / {PERSONA} persona ===")
    s.cmd("Page.navigate", {"url": "https://abrahamjuliot.github.io/creepjs/"})
    # It blocks the renderer hard while it measures; sleep through that without
    # touching the socket, then re-attach in case the connection went stale.
    time.sleep(45)
    s.reconnect()
    for _ in range(30):
        if s.eval("typeof window.Fingerprint === 'object' && window.Fingerprint !== null") is True:
            break
        time.sleep(3)
    else:
        print("  FAIL: window.Fingerprint never materialised")
        return
    fp = json.loads(s.eval(
        "JSON.stringify({h: window.Fingerprint.headless || {},"
        " lies: window.Fingerprint.lies || {}, trash: window.Fingerprint.trash || {}})"))
    h = fp["h"]
    for k in ("headlessRating", "stealthRating", "likeHeadlessRating"):
        if k in h:
            print(f"  {k:20} {h[k]}")
    print(f"  platformEstimate     {json.dumps(h.get('platformEstimate'))}")
    for bucket in ("headless", "stealth", "likeHeadless"):
        d = h.get(bucket)
        if isinstance(d, dict):
            hits = [k for k, v in d.items() if v]
            print(f"  {bucket:20} {len(hits)}/{len(d)}{': ' + ', '.join(hits) if hits else ''}")
    lies = fp["lies"].get("data", {})
    print(f"  lies                 {json.dumps(lies)}")
    print(f"  trash                {json.dumps([t.get('name') for t in fp['trash'].get('trashBin', [])])}")
    for bucket in ("headless", "stealth"):
        d = h.get(bucket)
        if isinstance(d, dict):
            hits = [k for k, v in d.items() if v]
            record(f"CreepJS {bucket} bucket clean", not hits,
                   ", ".join(hits) if hits else "")
    est = h.get("platformEstimate")
    if isinstance(est, list) and est and isinstance(est[0], dict):
        top = max(est[0], key=lambda k: est[0][k])
        want = {"windows": "Windows", "macos": "Mac"}[PERSONA]
        record("platformEstimate ranks the persona OS first", top == want,
               f"top={top} scores={json.dumps(est[0])}")
    # Informational: our own canvas/rects/measureText noise is what gets named
    # here, so a non-zero count is a known trade-off, not a regression.
    record("CreepJS lies", None, json.dumps(list(lies)) if lies else "none")


WORKER_JS = r"""
new Promise(res => {
  const code = `self.onmessage = () => {
    let r = null, v = null;
    try {
      const gl = new OffscreenCanvas(64,64).getContext('webgl');
      const e = gl && gl.getExtension('WEBGL_debug_renderer_info');
      if (e) { r = gl.getParameter(e.UNMASKED_RENDERER_WEBGL); v = gl.getParameter(e.UNMASKED_VENDOR_WEBGL); }
    } catch (err) {}
    self.postMessage({ua: navigator.userAgent, platform: navigator.platform,
      hc: navigator.hardwareConcurrency, renderer: r, vendor: v});
  };`;
  const w = new Worker(URL.createObjectURL(new Blob([code], {type:'text/javascript'})));
  w.onmessage = e => {
    const gl = document.createElement('canvas').getContext('webgl');
    const x = gl && gl.getExtension('WEBGL_debug_renderer_info');
    res(JSON.stringify({worker: e.data, main: {ua: navigator.userAgent,
      platform: navigator.platform, hc: navigator.hardwareConcurrency,
      renderer: x ? gl.getParameter(x.UNMASKED_RENDERER_WEBGL) : null,
      vendor: x ? gl.getParameter(x.UNMASKED_VENDOR_WEBGL) : null}}));
  };
  w.postMessage(1);
  setTimeout(() => res(JSON.stringify({error: 'worker timeout'})), 8000);
})"""


def worker_coherence(s: Session) -> None:
    """Worker realm vs main thread - CreepJS's hasHeadlessWorkerUA / hasBadWebGL."""
    print(f"\n=== worker vs main thread / {PERSONA} persona ===")
    d = json.loads(s.eval(WORKER_JS, await_promise=True))
    if "error" in d:
        print(f"  FAIL: {d['error']}")
        return
    m, w = d["main"], d["worker"]
    differ = []
    for label, a, b in (("userAgent", m["ua"], w["ua"]),
                        ("platform", m["platform"], w["platform"]),
                        ("hardwareConcurrency", m["hc"], w["hc"]),
                        ("WebGL renderer", m["renderer"], w["renderer"]),
                        ("WebGL vendor", m["vendor"], w["vendor"])):
        print(f"  {'MATCH ' if a == b else 'DIFFER'} {label}")
        if a != b:
            differ.append(label)
            print(f"      main={a!r}\n      worker={b!r}")
    record("worker realm matches main thread", not differ,
           "differs: " + ", ".join(differ) if differ else "")


VERDICT: list[tuple[str, bool | None, str]] = []


def record(name: str, ok: bool | None, detail: str = "") -> None:
    """ok True/False asserts; None means informational or unavailable."""
    VERDICT.append((name, ok, detail))


def body_json(s: Session, url: str, settle: int = 8):
    """Navigate to a page that renders JSON and parse it out of the body."""
    s.cmd("Page.navigate", {"url": url})
    time.sleep(settle)
    txt = s.eval("document.body ? document.body.innerText : ''") or ""
    start, end = txt.find("{"), txt.rfind("}")
    if start < 0 or end <= start:
        raise ValueError(f"no JSON in body ({len(txt)} chars)")
    return json.loads(txt[start:end + 1])


def are_you_a_bot(s: Session) -> None:
    """isAutomatedWithCDP and friends - the CDP-specific detector."""
    print(f"\n=== deviceandbrowserinfo / are_you_a_bot ===")
    try:
        d = body_json(s, "https://deviceandbrowserinfo.com/are_you_a_bot", settle=10)
    except Exception as e:
        print(f"  UNAVAILABLE: {e}")
        record("are_you_a_bot", None, "unavailable")
        return
    is_bot = d.get("isBot")
    details = d.get("details", d)
    print(f"  isBot {json.dumps(is_bot)}")
    flagged = [k for k, v in details.items() if v is True] if isinstance(details, dict) else []
    for k in sorted(details) if isinstance(details, dict) else []:
        print(f"    {'FLAG' if details[k] is True else '    '} {k}: {json.dumps(details[k])}")
    record("are_you_a_bot isBot", is_bot is not True,
           f"flagged: {', '.join(flagged)}" if flagged else "nothing flagged")


def cdp_mouse_leak(s: Session) -> None:
    """hasCDPMouseLeak: real mice coalesce hardware samples, CDP-injected moves do not.

    Dispatching the input over CDP is the whole point - that is how cuttle drives
    a page, so this measures the thing we actually do rather than a hypothetical.
    """
    print(f"\n=== deviceandbrowserinfo / CDP mouse leak ===")
    try:
        s.cmd("Page.navigate", {"url": "https://deviceandbrowserinfo.com/are_you_a_bot_interactions"})
        time.sleep(8)
        for i in range(24):
            s.cmd("Input.dispatchMouseEvent", {"type": "mouseMoved",
                                               "x": 120 + i * 11, "y": 200 + (i % 7) * 9,
                                               "button": "none", "clickCount": 0})
            time.sleep(0.05)
        time.sleep(6)
        txt = s.eval("document.body ? document.body.innerText : ''") or ""
        start, end = txt.find("{"), txt.rfind("}")
        d = json.loads(txt[start:end + 1]) if start >= 0 < end else {}
    except Exception as e:
        print(f"  UNAVAILABLE: {e}")
        record("hasCDPMouseLeak", None, "unavailable")
        return
    det = d.get("details", d)
    leak = det.get("hasCDPMouseLeak") if isinstance(det, dict) else None
    print(f"  hasCDPMouseLeak {json.dumps(leak)}   isBot {json.dumps(d.get('isBot'))}")
    if isinstance(det, dict):
        for k in sorted(k for k in det if "mouse" in k.lower() or "cdp" in k.lower()):
            print(f"    {k}: {json.dumps(det[k])}")
    record("hasCDPMouseLeak", leak is not True,
           "CDP-injected mouse moves are distinguishable" if leak is True else "")


def botstop(s: Session) -> None:
    """An independent live verdict. No signal breakdown by design."""
    print(f"\n=== botstop.io ===")
    try:
        s.cmd("Page.navigate", {"url": "https://botstop.io/"})
        time.sleep(15)
        txt = (s.eval("document.body ? document.body.innerText : ''") or "").strip()
    except Exception as e:
        print(f"  UNAVAILABLE: {e}")
        record("botstop", None, "unavailable")
        return
    if not txt:
        print("  UNAVAILABLE: empty body")
        record("botstop", None, "empty")
        return
    head = " | ".join(l.strip() for l in txt.splitlines() if l.strip())[:400]
    print(f"  {head}")
    low = txt.lower()
    bad = any(w in low for w in ("automated", "bot detected", "suspicious"))
    record("botstop verdict", not bad, head[:120])


def tcp_os(s: Session) -> None:
    """The OS guess from our TCP SYN. Structurally the container's Linux."""
    print(f"\n=== tcpip.incolumitas.com / classify ===")
    try:
        d = body_json(s, "https://tcpip.incolumitas.com/classify?detail=1", settle=8)
    except Exception as e:
        print(f"  UNAVAILABLE: {e}")
        record("tcp/ip OS", None, "unavailable")
        return
    guess = d.get("gs") or d.get("os") or d.get("guess") or d
    print(f"  {json.dumps(guess)[:300]}")
    # Informational on purpose: no Chromium patch reaches the TCP stack, so this
    # can never be made to agree with the persona from inside the browser.
    record("tcp/ip OS vs persona", None,
           f"structural mismatch expected: {json.dumps(guess)[:80]}")


def run_section(s: Session, name: str, fn) -> None:
    """No single section may end the run - that is the whole point of the tool."""
    try:
        fn(s)
    except Exception as e:
        print(f"\n  SECTION FAILED ({name}): {type(e).__name__}: {e}")
        record(name, None, f"section failed: {type(e).__name__}")
        try:
            s.reconnect()
        except Exception as re:
            print(f"  could not re-attach: {re}")


def summary() -> int:
    print("\n" + "=" * 64)
    print(f"POSTURE / {PERSONA} persona")
    print("=" * 64)
    failed = 0
    for name, ok, detail in VERDICT:
        mark = "PASS" if ok is True else ("FAIL" if ok is False else "info")
        failed += ok is False
        print(f"  [{mark}] {name}{'  - ' + detail if detail else ''}")
    print("\n  Not covered by any of the above, and not fixable from inside the")
    print("  browser: the TCP/IP fingerprint is the container's Linux under both")
    print("  personas, and nothing here exercises a real armed challenge. For that")
    print("  put a Turnstile widget or Bot Fight Mode on a zone you own.")
    print(f"\n  {failed} assertion(s) failed; scores above are informational.")
    return failed


def main() -> int:
    print(f"binary : {BINARY}")
    print(f"flags  : {len(production_args())} from {GOLDEN.name} ({ARCH})")
    s = Session([])
    try:
        for name, fn in (("persona basics", persona_basics),
                         ("worker coherence", worker_coherence),
                         ("CreepJS", creepjs),
                         ("are_you_a_bot", are_you_a_bot),
                         ("CDP mouse leak", cdp_mouse_leak),
                         ("botstop", botstop),
                         ("tcp/ip classify", tcp_os)):
            run_section(s, name, fn)
    finally:
        s.close()
    return summary()


if __name__ == "__main__":
    sys.exit(main())
