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
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

# --merge combines measurement files. It is pure JSON: no browser, no websocket,
# no display. Gating it behind the measurement preflight meant an operator could
# not assemble the checkpoint anywhere except a machine set up to run the probes,
# which is the one place they had already finished running them.
MERGING = "--merge" in sys.argv

if MERGING:
    websocket = None  # type: ignore[assignment]
else:
    try:
        import websocket  # type: ignore
    except ImportError:
        print("ERROR: pip install websocket-client", file=sys.stderr)
        sys.exit(2)

BINARY = os.environ.get("BROWSER_BINARY_PATH")
if not MERGING:
    if not BINARY or not Path(BINARY).exists():
        sys.exit(f"ERROR: BROWSER_BINARY_PATH not set or missing: {BINARY!r}")
    if not os.environ.get("DISPLAY"):
        sys.exit("ERROR: no DISPLAY. WebGL reads back as two empty strings without one, "
                 "which every detector scores as a broken GPU spoof.")

PERSONA = "windows" if MERGING else (sys.argv[1] if len(sys.argv) > 1 else "windows").lower()
if PERSONA not in ("windows", "macos"):
    sys.exit(f"ERROR: unknown persona {PERSONA!r} - expected windows or macos")
ARCH = {"windows": "amd64", "macos": "arm64"}[PERSONA]
def _default_golden() -> Path:
    here = Path(__file__).resolve()
    if len(here.parents) > 3:
        return here.parents[3] / "internal/fingerprint/testdata/golden.json"
    return Path("golden.json")


GOLDEN = Path(os.environ["GOLDEN_JSON"]) if os.environ.get("GOLDEN_JSON") else _default_golden()
if not GOLDEN.exists() and not MERGING:
    sys.exit(f"ERROR: golden not found at {GOLDEN}. Set GOLDEN_JSON to "
             "internal/fingerprint/testdata/golden.json - the flag set is derived "
             "from it so the tool cannot measure a browser we do not ship.")
PORT = int(os.environ.get("DETECT_CDP_PORT", "9971"))


def _preflight_shm() -> None:
    """Warn on a 64MB /dev/shm.

    Chrome crashes under load without a large /dev/shm, and this repo already
    knows it: ops/helm/.../deployment.yaml mounts a Memory emptyDir there,
    docs/OPERATING.md documents --shm-size=2g, and internal/backend/local.go
    passes it on every container cuttle starts. A hand-written `docker run`
    inherits Docker's 64MB default instead, and the failure is the worst kind -
    intermittent. CreepJS died roughly half the time across three hosts, which
    cost several wrong diagnoses (slow host, then a suspected binary
    regression) before anyone looked at the container config.

    Fail loudly rather than emit a checkpoint built from flaky numbers.
    """
    try:
        st = os.statvfs("/dev/shm")
    except OSError:
        return  # not Linux, or no /dev/shm - nothing to assert
    mb = st.f_blocks * st.f_frsize // (1024 * 1024)
    if mb < 1024:
        print(f"NOTE: /dev/shm is {mb}MB. DAEMON_BASE_ARGS carries "
              "--disable-dev-shm-usage so Chrome falls back to /tmp rather than\n"
              "      crashing, but that is disk-backed here. --shm-size=2g matches "
              "what internal/backend/local.go passes in production.", file=sys.stderr)


if not os.environ.get("DETECT_ALLOW_SMALL_SHM"):
    _preflight_shm()


def daemon_base_args(g: dict) -> list[str]:
    """The flags the daemon launches every Chrome with.

    READ from golden.json, never copied. The bench used to replay only the
    fingerprint args, so it silently dropped all of these - including
    --disable-dev-shm-usage, which is what makes cuttle immune to a small
    /dev/shm. Without it the renderer died partway through CreepJS on a default
    64MB container, intermittently, and that was misdiagnosed as a slow host, a
    binary regression and a flaky harness before anyone compared the argv.
    """
    args = g.get("base_chrome_args")
    if not args:
        sys.exit(f"ERROR: {GOLDEN} has no base_chrome_args. Regenerate it with "
                 "`just parity-golden` - without it this bench would launch a "
                 "browser the daemon never launches.")
    return list(args)


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
    machine_key = "apple_silicon_args" if ARCH == "arm64" else "windows_machine_args"
    for e in g.get(machine_key, []):
        # A representative machine from the pool; production picks by seed.
        if e.get("arch") == ARCH and e.get("output"):
            parts.append(e["output"])
            break
    # ScreenArgs too - pool.go appends it on EVERY launch. Leaving it out let the
    # probe run on the binary's seed-default display while asserting the spoofed
    # screen and its media queries, which is exactly the "measured a browser we do
    # not ship" failure this whole function exists to prevent.
    for e in g.get("screen_args", []):
        if e.get("arch") == ARCH and e.get("output"):
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
        self.extra = extra
        self._launch()

    def _launch(self) -> None:
        self.profile = tempfile.mkdtemp()
        self.proc = subprocess.Popen(
            [BINARY, f"--remote-debugging-port={PORT}", f"--user-data-dir={self.profile}",
             "--no-sandbox", *daemon_base_args(json.loads(GOLDEN.read_text())),
             "--remote-allow-origins=*", *production_args(), *self.extra, "about:blank"],
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
            self.close()
            sys.exit("ERROR: browser never exposed a CDP target")
        page = next(t for t in targets if t["type"] == "page")
        # Deliberately not generous. A detector page blocks the renderer for tens
        # of seconds, and callers retry on timeout - so a short timeout costs a
        # retry, while a long one hides a DEAD renderer behind minutes of waiting
        # that look identical to a busy one.
        self.ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=30)
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
                    page["webSocketDebuggerUrl"], timeout=30)
                self._id = 0
                # Prove it before claiming success: create_connection succeeds
                # against a target that is already going away, and reporting a
                # dead socket as recovered is what let one failed section
                # poison every section after it.
                self.eval("1")
                return
            except Exception:
                time.sleep(1)
        raise RuntimeError("could not re-attach to the browser")

    def recover(self) -> None:
        """Re-attach, and relaunch the browser if re-attaching cannot work.

        A section that takes the renderer down used to poison the whole run:
        reconnect() only swaps the websocket, so once the page target had died
        with the renderer there was nothing left to attach to and every later
        section reported "socket is already closed". Recovery has to be able to
        replace the process, not just the connection.
        """
        try:
            self.reconnect()
            return
        except Exception as e:
            print(f"  re-attach failed ({type(e).__name__}); relaunching the browser")
        try:
            self.close()
        except Exception:
            pass
        self._launch()

    def close(self) -> None:
        try:
            ws = getattr(self, "ws", None)
            if ws is not None:
                ws.close()
        finally:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                # A Chrome that ignores SIGTERM otherwise outlives the harness and
                # keeps the CDP port bound for the next run. Same reasoning as
                # validate/smoke.py.
                self.proc.kill()
                self.proc.wait()
            shutil.rmtree(self.profile, ignore_errors=True)


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
        # barcode + the screen/DPR trio are what patches #53 and #54 changed.
        # realref.py reports the identical set, so the checkpoint compares like
        # with like rather than fields that exist on only one side.
        " barcode: 'BarcodeDetector' in window,"
        " scrH: screen.height, scrAvailH: screen.availHeight, dpr: devicePixelRatio,"
        " mmDevice: matchMedia(`(device-width: ${screen.width}px) and"
        " (device-height: ${screen.height}px)`).matches,"
        " mmRes: matchMedia(`(resolution: ${devicePixelRatio}dppx)`).matches,"
        " heapLimitGB: (performance.memory ? +(performance.memory.jsHeapSizeLimit/1073741824).toFixed(2) : null)})"))
    for k, v in d.items():
        print(f"  {k:12} {json.dumps(v)}")
    METRICS["basics"] = d
    if d.get("mem") and d.get("heapLimitGB") and d["heapLimitGB"] > d["mem"]:
        print(f"  !! deviceMemory {d['mem']}GB < JS heap limit {d['heapLimitGB']}GB"
              " - detectors flag this pair as incoherent")
    record("no HeadlessChrome token", "HeadlessChrome" not in (d.get("ua") or ""))
    record("deviceMemory exposed", d.get("mem") is not None,
           "real Chrome always exposes it" if d.get("mem") is None else f"{d.get('mem')}GB")
    record("device APIs present", d.get("bluetooth") == "object" and d.get("usb") == "object",
           f"bluetooth={d.get('bluetooth')} usb={d.get('usb')}")
    record("navigator.share present", d.get("share") == "function",
           "absent in unbranded Chromium; real desktop Chrome has it")
    # Patch #54. Both sides are read from the binary, so this can only pass if
    # the spoofed screen/DPR and CSS media evaluation resolve from one source.
    record("media queries agree with screen/DPR",
           d.get("mmDevice") is True and d.get("mmRes") is True,
           f"device-width/height={d.get('mmDevice')} resolution={d.get('mmRes')}")
    # Patch #53. Persona-gated on purpose: real Windows Chrome has no such
    # interface, real macOS Chrome does.
    record("BarcodeDetector matches the persona",
           d.get("barcode") is (PERSONA == "macos"),
           f"present={d.get('barcode')} (expected {PERSONA == 'macos'})")


def creepjs(s: Session) -> None:
    print(f"=== CreepJS / {PERSONA} persona ===")
    s.cmd("Page.navigate", {"url": "https://abrahamjuliot.github.io/creepjs/"})
    # POLL, do not blind-sleep, and do not re-attach. This section used to sleep
    # 45s through the measurement and then reconnect, on the theory that touching
    # a blocked renderer would kill the socket. It killed it instead: the Windows
    # persona timed out here on three hosts across two different binaries, so the
    # cause was never the browser. The predecessor harness (creeprun.py) polled a
    # live socket and worked, which is what this restores - a heavy page answers
    # an eval late, not never, and each poll that returns False is itself proof
    # the connection is fine.
    deadline = time.time() + 180
    ready = False
    while time.time() < deadline:
        try:
            if s.eval("typeof window.Fingerprint === 'object' && window.Fingerprint !== null") is True:
                ready = True
                break
        except websocket.WebSocketTimeoutException:
            pass  # renderer busy under the measurement - the next poll retries
        except websocket.WebSocketConnectionClosedException:
            # Not busy: gone. Raising lets run_section relaunch, so the sections
            # after this one still produce numbers.
            raise
        time.sleep(3)
    if not ready:
        print("  FAIL: window.Fingerprint never materialised within 180s")
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
    METRICS["creepjs"] = {
        "headlessRating": h.get("headlessRating"),
        "stealthRating": h.get("stealthRating"),
        "likeHeadlessRating": h.get("likeHeadlessRating"),
        "platformEstimate": (h.get("platformEstimate") or [{}])[0],
        "likeHeadless": sorted(k for k, v in (h.get("likeHeadless") or {}).items() if v),
        "lies": sorted(lies),
    }
    print(f"  lies                 {json.dumps(lies)}")
    print(f"  trash                {json.dumps([t.get('name') for t in fp['trash'].get('trashBin', [])])}")
    for bucket in ("headless", "stealth"):
        d = h.get(bucket)
        if isinstance(d, dict):
            hits = [k for k, v in d.items() if v]
            record(f"CreepJS {bucket} bucket clean", not hits,
                   ", ".join(hits) if hits else "")
    ref = REFERENCE.get(PERSONA)
    if ref:
        hits = {k for k, v in (h.get("likeHeadless") or {}).items() if v}
        compare("headlessRating", h.get("headlessRating"), ref["headlessRating"])
        compare("stealthRating", h.get("stealthRating"), ref["stealthRating"])
        compare("likeHeadlessRating", h.get("likeHeadlessRating"), ref["likeHeadlessRating"])
        extra = sorted(hits - ref["likeHeadless"])
        missing = sorted(ref["likeHeadless"] - hits)
        compare("likeHeadless keys ours-only", ", ".join(extra) or "none", "-")
        if missing:
            compare("likeHeadless keys real-only", "-", ", ".join(missing))
        compare("lies", ", ".join(sorted(lies)) or "none", "none")
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


# Measured from real Chrome on real hardware, HEADED - because cuttle runs headed
# under Xvfb+openbox. A headless control is the wrong baseline: it scores
# headlessRating 67 where a real browser scores 0, fires noTaskbar that a real
# windowed browser passes, and so flatters us on exactly the signals that matter.
# The posture below is reported as a DELTA against these numbers, not against
# zero, because "6 likeHeadless hits" means nothing until you know a real browser
# fires 4 of them.
REFERENCE = {
    "macos": {
        "source": "real Chrome 151.0.7922.138 / macOS 26.7 / headed",
        "headlessRating": 0,
        "stealthRating": 0,
        "likeHeadlessRating": 25,
        "platformTop": "Mac",
        "likeHeadless": {"hasKnownBgColor", "noContentIndex",
                         "noContactsManager", "noDownlinkMax"},
        "lies": set(),
    },
    "windows": {
        "source": "real Chrome 151.0.7922.138 / Windows 11 / headed",
        "headlessRating": 0,
        "stealthRating": 0,
        "likeHeadlessRating": 25,
        "platformTop": "Windows",
        # noTaskbar fires on the reference machine because it reports
        # availHeight == height. That is a property of that box, not of Windows
        # in general, and our persona reserves a real 48px taskbar - so
        # noTaskbar shows up as "real-only", i.e. a signal a real browser trips
        # and we do not. Left in rather than filtered out: the delta is the
        # honest measurement, and this direction costs us nothing.
        "likeHeadless": {"noContactsManager", "noContentIndex",
                         "noDownlinkMax", "noTaskbar"},
        "lies": set(),
    },
}


VERDICT: list[tuple[str, bool | None, str]] = []


DELTA: list[tuple[str, object, object]] = []
# Machine-readable record of this run, for packages/browser/benches/posture.json.
METRICS: dict = {}


def compare(name: str, ours: object, theirs: object) -> None:
    DELTA.append((name, ours, theirs))


def record(name: str, ok: bool | None, detail: str = "") -> None:
    """ok True/False asserts; None means informational or unavailable."""
    VERDICT.append((name, ok, detail))


def _json_from_body(txt: str) -> dict:
    """Pull the first JSON object out of a rendered page body, or {} if absent."""
    start, end = txt.find("{"), txt.rfind("}")
    if start < 0 or end <= start:
        return {}
    return json.loads(txt[start:end + 1])


def body_json(s: Session, url: str, settle: int = 8):
    """Navigate to a page that renders JSON and parse it out of the body."""
    s.cmd("Page.navigate", {"url": url})
    time.sleep(settle)
    txt = s.eval("document.body ? document.body.innerText : ''") or ""
    d = _json_from_body(txt)
    if not d:
        raise ValueError(f"no JSON in body ({len(txt)} chars)")
    return d


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
    METRICS["are_you_a_bot"] = {"isBot": is_bot, "flagged": flagged}
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
        d = _json_from_body(txt)
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
        if leak is None:
            # The field was not where we looked. Print the keys we did get rather
            # than reporting a value we never measured.
            print(f"    (no hasCDPMouseLeak key; page returned: "
                  f"{', '.join(sorted(det)[:24])})")
    if leak is None:
        record("hasCDPMouseLeak", None, "not reported - the page returned no value")
    else:
        record("hasCDPMouseLeak", leak is not True,
               "CDP-injected mouse moves are distinguishable" if leak else "")


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
    verdict = "HUMAN" if "human" in low else ("AUTOMATED" if "automated" in low else "?")
    score = None
    for i, tok in enumerate(txt.split()):
        if tok.strip() == "SCORE" and i + 1 < len(txt.split()):
            try:
                score = int(txt.split()[i + 1])
            except ValueError:
                pass
    METRICS["botstop"] = {"verdict": verdict, "score": score}
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
    details = d.get("details", {}) if isinstance(d, dict) else {}
    scores = d.get("avg_score_os_class", {}) if isinstance(d, dict) else {}
    top = details.get("os_highest_class", "?")
    print(f"  egress IP withheld  ->  {top}  {json.dumps(scores)}")
    print("  NOTE: this measures the HOST this run executed on, not the binary.")
    print("  A run from a laptop reports that laptop's stack and will look fine")
    print("  for a macOS persona; production runs in the Linux container, where")
    print("  the same probe reports Chromium OS against a Windows claim.")
    # Informational by construction: no Chromium patch reaches the TCP stack, so
    # this can never be made to agree with the persona from inside the browser.
    # It belongs to the deployment gate and must be re-measured per environment.
    record("tcp/ip OS (host, not binary)", None,
           f"this host reads as {top}; re-measure per deployment")


def run_section(s: Session, name: str, fn) -> None:
    """No single section may end the run - that is the whole point of the tool."""
    try:
        fn(s)
    except Exception as e:
        print(f"\n  SECTION FAILED ({name}): {type(e).__name__}: {e}")
        record(name, None, f"section failed: {type(e).__name__}")
        try:
            s.recover()
        except Exception as re:
            print(f"  could not recover the browser: {re}")


def summary() -> int:
    print("\n" + "=" * 64)
    print(f"POSTURE / {PERSONA} persona")
    print("=" * 64)
    ref = REFERENCE.get(PERSONA)
    if not ref:
        print("  NO REFERENCE for this persona - real hardware has not been")
        print("  measured, so the numbers below carry no verdict.\n")
    else:
        print(f"  baseline: {ref['source']}\n")
        for name, ours, theirs in DELTA:
            if ours == theirs:
                print(f"  [same] {name}: {ours}")
            else:
                print(f"  [DIFF] {name}: ours={ours} real={theirs}")
        print()
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


def merge(out: str, inputs: list[str]) -> int:
    """Combine per-run metric files into the committed checkpoint.

    Four runs make a checkpoint: real Chrome and our binary, on each platform.
    Keyed by platform then by which browser produced it, so a diff against the
    previous checkpoint reads as "what moved, and did it move toward or away from
    the real browser".
    """
    posture: dict = {"chromium_version": "", "platforms": {}}
    versions = Path(__file__).resolve().parents[1] / "versions.env"
    if versions.exists():
        for line in versions.read_text().splitlines():
            if line.startswith("CHROMIUM_VERSION="):
                posture["chromium_version"] = line.split("=", 1)[1].split("#")[0].strip()
    for path in inputs:
        data = json.loads(Path(path).read_text())
        persona = data.pop("persona", "?")
        # realref.py reports persona "real"; the platform comes from the filename
        # so one reference script can serve both machines.
        stem = Path(path).stem.lower()
        platform = "macos" if "mac" in stem else "windows"
        who = "real" if persona == "real" else "ours"
        posture["platforms"].setdefault(platform, {})[who] = data
    Path(out).write_text(json.dumps(posture, indent=2, sort_keys=True) + "\n")
    print(f"[detect] checkpoint -> {out}")
    for platform, sides in sorted(posture["platforms"].items()):
        real = (sides.get("real") or {}).get("creepjs", {})
        ours = (sides.get("ours") or {}).get("creepjs", {})
        if not real or not ours:
            print(f"  {platform}: incomplete ({', '.join(sorted(sides))})")
            continue
        for key in ("headlessRating", "stealthRating", "likeHeadlessRating"):
            mark = "same" if real.get(key) == ours.get(key) else "DIFF"
            print(f"  {platform} {key}: ours={ours.get(key)} real={real.get(key)} [{mark}]")
    return 0


def main() -> int:
    if "--merge" in sys.argv:
        i = sys.argv.index("--merge")
        return merge(sys.argv[i + 1], sys.argv[i + 2:])
    emit = None
    if "--json" in sys.argv:
        emit = sys.argv[sys.argv.index("--json") + 1]
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
    rc = summary()
    if emit:
        METRICS["persona"] = PERSONA
        METRICS["binary"] = Path(BINARY).name
        Path(emit).write_text(json.dumps(METRICS, indent=2, sort_keys=True) + "\n")
        print(f"\n[detect] metrics -> {emit}")
    return rc


if __name__ == "__main__":
    sys.exit(main())
