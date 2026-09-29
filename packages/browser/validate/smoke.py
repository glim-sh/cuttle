#!/usr/bin/env python3
# SPDX: MIT. Adapted from clark-browser tests/linux_smoke.py and owned here.
"""In-container behavioral smoke test for a built stealth-Chromium binary.

Talks CDP directly (HTTP + WebSocket) via pure-python websocket-client. Asserts
the JS/UA-CH/WebGL/canvas/audio surface against a per-persona expectation set.

cuttle ships exactly two personas, selected by the binary's own arch (see
packages/cuttle/internal/fingerprint/args.go - personaIsMacOS):
  amd64 -> windows (Win32)
  arm64 -> macos   (MacIntel)
so the persona is derived from BUILD_ARCH, not from whether a fonts dir happens
to be set. SMOKE_PROFILE overrides it only to test the other persona on purpose.

UA-CH architecture is NOT a persona trait: the patch series derives it from the
compile target (__aarch64__), so every persona built from one binary reports the
same value. It is asserted against BUILD_ARCH below, not hardcoded per persona.

Binary path: BROWSER_BINARY_PATH.
Exit code is the number of failed assertions; 0 = full pass.
"""
from __future__ import annotations

import json
import threading
import os
import platform
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Iterator
from xml.sax.saxutils import escape

try:
    import websocket  # type: ignore  # websocket-client
except ImportError:
    print("ERROR: pip install websocket-client", file=sys.stderr)
    sys.exit(2)

BINARY = os.environ.get("BROWSER_BINARY_PATH")
if not BINARY or not Path(BINARY).exists():
    print(f"ERROR: BROWSER_BINARY_PATH not set or missing: {BINARY!r}", file=sys.stderr)
    sys.exit(2)

# The smoke runs the binary natively, so the host arch is the build arch.
BUILD_ARCH = "arm" if platform.machine().lower() in ("aarch64", "arm64") else "x86"

VERSIONS = Path(__file__).resolve().parent.parent / "versions.env"


def versions_env(key: str) -> str:
    """Read one key from versions.env, the single source of version truth."""
    for line in VERSIONS.read_text().splitlines():
        stripped = line.strip()
        if stripped.startswith(f"{key}="):
            return stripped.split("=", 1)[1].split("#", 1)[0].strip()
    print(f"ERROR: {key} missing from {VERSIONS}", file=sys.stderr)
    sys.exit(2)


# The full 4-part build appears only in UA-CH; navigator.userAgent carries the
# reduced form. Deriving both from one value is not cosmetic: with a fingerprint
# persona active the binary builds navigator.userAgent from the major of
# --fingerprint-brand-version (patch 0006), the same value UA-CH comes from, while
# the HTTP header is --user-agent verbatim. The two only agree if both are cut
# from this one version.
CHROMIUM_VERSION = versions_env("CHROMIUM_VERSION")
CHROME_UA_VERSION = CHROMIUM_VERSION.split(".", 1)[0] + ".0.0.0"
# Persona OS versions. Mirror ForkParityArgs in packages/cuttle/internal/fingerprint/args.go;
# TestPersonaVersionsMatchSmoke asserts they agree.
WINDOWS_PLATFORM_VERSION = "19.0.0"
MACOS_PLATFORM_VERSION = "26.7.0"

# Upstream indexes the GREASE brand by the major version (see
# components/embedder_support/user_agent_utils.cc). Asserting the literal string
# is what catches a hardcoded value: patch 0007's Blink half shipped a frozen
# "Not A(Brand" for every version, contradicting our own Sec-CH-UA header.
_GREASY = [" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"]
_MAJOR = int(CHROMIUM_VERSION.split(".", 1)[0])
GREASE_BRAND = f"Not{_GREASY[_MAJOR % 11]}A{_GREASY[(_MAJOR + 1) % 11]}Brand"

PORT = int(os.environ.get("BROWSER_CDP_PORT", "9444"))
PROFILE = Path("/tmp/stealth-smoke-profile")
WINDOWS_CORE_FONTS = ("Arial", "Segoe UI", "Calibri")
MACOS_CORE_FONTS = ("Helvetica Neue", "Helvetica", "Menlo")
# The free fonts our packs are renamed FROM. A persona that exposes these is
# leaking its substitutes - the failure 50-block-linux-aliases.conf exists to
# stop (Skia's metric-equivalence table accepts a Linux family against our
# renamed one, which no fontconfig alias can intercept).
SUBSTITUTE_SOURCE_FONTS = ("Liberation Sans", "DejaVu Sans", "Carlito", "Caladea")
# Must never resolve. Guards the detector itself: document.fonts.check() reports
# true for ANY family, so a probe built on it silently passes with no fonts at
# all. If this reads present, the measurement is broken, not the font set.
SENTINEL_FONT = "cuttleNoSuchFamily7Z"
FONTS_DIR = (os.environ.get("BROWSER_FONTS_DIR") or "").strip()
SMOKE_PROFILE = os.environ.get(
    "SMOKE_PROFILE", "macos" if BUILD_ARCH == "arm" else "windows"
).strip().lower()
if SMOKE_PROFILE not in ("windows", "macos"):
    print(f"ERROR: unsupported SMOKE_PROFILE={SMOKE_PROFILE!r} (windows|macos)", file=sys.stderr)
    sys.exit(2)


# Patch #53. What real Chrome on macOS returns from
# BarcodeDetector.getSupportedFormats(): the Vision symbologies mapped through
# ToBarcodeFormat() and collected into a base::flat_set, so the list arrives
# sorted by the mojom BarcodeFormat enum ordinal. ORDER IS PART OF THE VALUE -
# comparing as a list, not a set, is deliberate. codabar and upc_a are absent
# because Vision exposes no symbology that maps onto them.
MACOS_BARCODE_FORMATS = [
    "aztec", "code_128", "code_39", "code_93", "data_matrix", "ean_13",
    "ean_8", "itf", "pdf417", "qr_code", "upc_e",
]

def daemon_base_args() -> list[str]:
    """The flags the daemon launches every Chrome with (baseChromeArgs).

    READ from golden.json, never copied. Composing its own list is how this gate
    ended up validating a browser we do not ship: it was missing
    --ignore-gpu-blocklist, so WebGL could be blocklisted and the WebGL
    assertion would then compare two empty strings and still "match".
    """
    path = os.environ.get("GOLDEN_JSON") or str(
        Path(__file__).resolve().parents[3] / "packages/cuttle/internal/fingerprint/testdata/golden.json")
    try:
        args = json.loads(Path(path).read_text()).get("base_chrome_args")
    except OSError as e:
        print(f"ERROR: cannot read golden at {path}: {e}", file=sys.stderr)
        sys.exit(2)
    if not args:
        print(f"ERROR: {path} has no base_chrome_args - regenerate with "
              "`just parity-golden`", file=sys.stderr)
        sys.exit(2)
    return list(args)


VOICES_JS = """
    new Promise(res => {
      const s = speechSynthesis;
      const sync = s.getVoices().length;
      let events = 0;
      s.onvoiceschanged = () => { events++; };
      setTimeout(() => {
        const v = s.getVoices();
        const d = v.find(x => x.default);
        res({
          sync, events, total: v.length,
          local: v.filter(x => x.localService).length,
          def: d ? d.name : null,
          defCount: v.filter(x => x.default).length,
          uriEqName: v.every(x => x.voiceURI === x.name),
        });
      }, 5000);
    })
"""


class TrustedPageHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(b"<!doctype html><title>smoke</title>")

    def log_message(self, format: str, *args: object) -> None:
        return


def _next_id(state: dict) -> int:
    state["id"] += 1
    return state["id"]


def cdp_eval(expr: str) -> str:
    with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/list", timeout=5) as r:
        targets = json.loads(r.read())
    page = next((t for t in targets if t.get("type") == "page"), None)
    if not page:
        with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/new?about:blank", timeout=5) as r:
            page = json.loads(r.read())
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=10)
    state = {"id": 0}
    try:
        ws.send(json.dumps({
            "id": _next_id(state),
            "method": "Runtime.evaluate",
            "params": {"expression": expr, "returnByValue": True, "awaitPromise": True},
        }))
        while True:
            msg = json.loads(ws.recv())
            if msg.get("id") == state["id"]:
                if "error" in msg:
                    return f"<error: {msg['error'].get('message', '?')}>"
                r = msg.get("result", {}).get("result", {})
                if "value" in r:
                    return json.dumps(r["value"])
                return json.dumps(r.get("description", "<undefined>"))
    finally:
        ws.close()


def cdp_navigate(url: str) -> None:
    with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/list", timeout=5) as r:
        targets = json.loads(r.read())
    page = next((t for t in targets if t.get("type") == "page"), None)
    if not page:
        return
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=10)
    try:
        ws.send(json.dumps({"id": 1, "method": "Page.navigate", "params": {"url": url}}))
        while True:
            if json.loads(ws.recv()).get("id") == 1:
                break
    finally:
        ws.close()


def _arg_value(args: tuple[str, ...], key: str) -> str | None:
    prefix = f"{key}="
    for arg in args:
        if arg.startswith(prefix):
            return arg.split("=", 1)[1]
    return None


def _fontconfig_env(fonts_dir: str | None) -> dict[str, str]:
    if not fonts_dir:
        return {}
    config_path = PROFILE / "fontconfig-smoke.conf"
    config_path.write_text(
        '<?xml version="1.0"?>\n'
        '<!DOCTYPE fontconfig SYSTEM "fonts.dtd">\n'
        '<fontconfig>\n'
        '  <include ignore_missing="yes">/etc/fonts/fonts.conf</include>\n'
        f"  <dir>{escape(fonts_dir)}</dir>\n"
        "</fontconfig>\n"
    )
    return {"FONTCONFIG_FILE": os.fspath(config_path)}


@contextmanager
def trusted_local_page() -> Iterator[tuple[str, str]]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), TrustedPageHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    host, port = server.server_address
    origin = f"http://{host}:{port}"
    try:
        yield f"{origin}/", origin
    finally:
        server.shutdown()
        server.server_close()


@contextmanager
def launch(*args: str) -> Iterator[None]:
    # The previous launch's Chrome children (zygote/renderers) can still be
    # flushing into the profile when the next one starts, so a bare rmtree races
    # them and dies with "Directory not empty". Retry briefly; the window is
    # milliseconds natively but widens under emulation or on a loaded CI host.
    for attempt in range(20):
        try:
            shutil.rmtree(PROFILE)
            break
        except FileNotFoundError:
            break
        except OSError:
            if attempt == 19:
                raise
            time.sleep(0.25)
    PROFILE.mkdir(parents=True)
    cmd = [
        BINARY,
        "--headless=new", "--no-sandbox", "--test-type", "--use-mock-keychain",
        f"--remote-debugging-port={PORT}",
        "--remote-debugging-address=127.0.0.1",
        "--remote-allow-origins=*",
        f"--user-data-dir={PROFILE}",
        # The daemon's own launch flags, read from the golden. Without them the
        # gate was missing --ignore-gpu-blocklist among others, so it validated a
        # browser we do not ship.
        *daemon_base_args(),
        *args,
        "about:blank",
    ]
    env = os.environ.copy()
    env.update(_fontconfig_env(_arg_value(args, "--fingerprint-fonts-dir")))
    proc = subprocess.Popen(
        cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env
    )
    try:
        for _ in range(40):
            # A dead child never opens the port; without this the loop burns the
            # full budget and reports "CDP never came up" with no cause.
            if proc.poll() is not None:
                raise RuntimeError(f"browser exited before CDP came up (rc={proc.returncode})")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/version", timeout=1) as r:
                    if r.status == 200:
                        break
            except Exception:
                time.sleep(0.3)
        else:
            raise RuntimeError("CDP never came up")
        yield
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()  # reap: an unreaped child can still hold PORT for the next launch


failures: list[str] = []
def expect(label: str, actual: str, predicate, expected_desc: str) -> None:
    ok = predicate(actual)
    mark = "PASS" if ok else "FAIL"
    print(f"  [{mark}] {label}: {actual}  (expected: {expected_desc})")
    if not ok:
        failures.append(f"{label}: got {actual!r}, expected {expected_desc}")


def json_ok(actual: str, predicate) -> bool:
    try:
        return bool(predicate(json.loads(actual)))
    except Exception:
        return False


def _font_profile_args(seed: str) -> tuple[list[str], dict]:
    if SMOKE_PROFILE == "windows":
        windows_args = [
            f"--fingerprint={seed}",
            "--fingerprint-platform=windows",
            f"--fingerprint-platform-version={WINDOWS_PLATFORM_VERSION}",
            "--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            f"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/{CHROME_UA_VERSION} Safari/537.36",
        ]
        if FONTS_DIR:
            windows_args.append(f"--fingerprint-fonts-dir={FONTS_DIR}")
        # ForkParityArgs: Windows-only, so canvas takes the analytic-AA path.
        windows_args.append("--msaa_is_slow")
        return windows_args, {
            "label": "Windows", "navigator_platform": "Win32",
            "ua_marker": "Windows NT 10.0", "ua_ch_platform": "Windows",
            "ua_ch_platform_version": WINDOWS_PLATFORM_VERSION, "architecture": BUILD_ARCH,
            "dpr": 1,
            # Production shape: ScreenArgs + WindowsMachineArgs in
            # packages/cuttle/internal/fingerprint/args.go emit these on every launch, so the
            # gate must drive the same path rather than the seed-default one.
            "screen": (1366, 768, 48), "device_memory": 16,
            "barcode_detector": False,
            # Real Windows Chrome has no BarcodeDetector (measured on real
            # hardware), so the persona must not enable it.
            "blink_features": "WebShare",
            # Transcribed from real Chrome 151 on Windows 11: 3 OneCore local
            # voices plus the 19 Google network voices, delivered in two
            # voiceschanged events (network first, then local).
            "voices_total": 22, "voices_local": 3, "voices_events": 2,
            "voices_default": "Microsoft David - English (United States)",
        }
    if SMOKE_PROFILE == "macos":
        # macOS UA is the frozen Intel Mac OS X 10_15_7 token (real Mac Chrome).
        macos_args = [
            f"--fingerprint={seed}",
            "--fingerprint-platform=macos",
            f"--fingerprint-platform-version={MACOS_PLATFORM_VERSION}",
            "--user-agent=Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
            f"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/{CHROME_UA_VERSION} Safari/537.36",
        ]
        if FONTS_DIR:
            macos_args.append(f"--fingerprint-fonts-dir={FONTS_DIR}")
        # Must mirror ForkParityArgs: clark's platform=macos GPU default is an
        # Intel-Mac card, which contradicts architecture=arm on the arm64 build.
        macos_args += [
            "--fingerprint-gpu-vendor=Google Inc. (Apple)",
            "--fingerprint-gpu-renderer=ANGLE (Apple, ANGLE Metal Renderer: Apple M2, Unspecified Version)",
        ]
        return macos_args, {
            "label": "macOS", "navigator_platform": "MacIntel",
            "ua_marker": "Intel Mac OS X 10_15_7", "ua_ch_platform": "macOS",
            "ua_ch_platform_version": MACOS_PLATFORM_VERSION, "architecture": BUILD_ARCH,
            "dpr": 2,
            # Production shape: ScreenArgs + AppleSiliconArgs. 1710x1112 is a
            # MacBook Pro 14" logical resolution, which is only coherent at
            # DPR 2 - the persona's screen sizes and its DPR agree by design.
            "screen": (1710, 1112, 95), "device_memory": 16,
            "barcode_detector": True,
            "blink_features": "WebShare,BarcodeDetector",
            # Real Chrome 151 on macOS 26.7 - the same machine MACOS_PLATFORM_VERSION
            # describes. 180 local plus the 19 Google network voices, one event.
            "voices_total": 199, "voices_local": 180, "voices_events": 1,
            "voices_default": "Samantha",
        }


# --- Referrer and UA-CH wire headers (patches 0040, 0019) -------------------
# The binary must ship stock Chrome here on its own: ungoogled's
# MinimalReferrers / NoCrossOriginReferrers and RemoveClientHints stay off. Two
# ports are two origins, and 127.0.0.1 is potentially trustworthy, so UA-CH is
# sent without any secure-origin flag.
class HeaderEchoHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path.startswith("/echo"):
            body = json.dumps({
                k.lower(): v for k, v in self.headers.items()
                if k.lower() == "referer" or k.lower().startswith("sec-ch-ua")
            }).encode()
            content_type = "application/json"
        else:
            body = b"<!doctype html><title>headers</title>"
            content_type = "text/html; charset=utf-8"
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: object) -> None:
        return


@contextmanager
def header_echo_origins() -> Iterator[tuple[str, str]]:
    servers = [ThreadingHTTPServer(("127.0.0.1", 0), HeaderEchoHandler) for _ in range(2)]
    for server in servers:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        a, b = (f"http://127.0.0.1:{s.server_address[1]}" for s in servers)
        yield a, b
    finally:
        for server in servers:
            server.shutdown()
            server.server_close()


def check_referrer_and_ua_ch_headers(args: list[str], profile: dict) -> None:
    print("\n=== referrer + UA-CH wire headers (stock Chrome defaults) ===")
    with header_echo_origins() as (origin_a, origin_b), launch(*args):
        time.sleep(0.5)
        page = f"{origin_a}/page?q=1"
        cdp_navigate(page)
        time.sleep(0.5)
        seen = cdp_eval(f"""
            (async () => ({{
              same: await (await fetch('{origin_a}/echo')).json(),
              cross: await (await fetch('{origin_b}/echo')).json(),
            }}))()
        """)
        expect("Referer: full URL same-origin, origin-only cross-origin", seen,
               lambda v: json_ok(v, lambda s:
                   s["same"].get("referer") == page and
                   s["cross"].get("referer") == f"{origin_a}/"),
               f"same {page}, cross {origin_a}/")
        platform_header = f'"{profile["ua_ch_platform"]}"'
        expect("Sec-CH-UA low-entropy headers on the wire", seen,
               lambda v: json_ok(v, lambda s:
                   '"Google Chrome"' in s["same"].get("sec-ch-ua", "") and
                   GREASE_BRAND in s["same"].get("sec-ch-ua", "") and
                   s["same"].get("sec-ch-ua-mobile") == "?0" and
                   s["same"].get("sec-ch-ua-platform") == platform_header),
               f"sec-ch-ua with Google Chrome + {GREASE_BRAND}, mobile ?0, "
               f"platform {platform_header}")


# --- Audio (patches #26 and #61) ------------------------------------------------
# Real Chrome 154's default AudioContext: 48 kHz on both personas, with the
# default output device's buffer as baseLatency (Windows 480 frames; macOS 256,
# measured on a real Mac). A device-less container otherwise reports 44.1 kHz.
AUDIO_BASE_LATENCY = {"windows": 480 / 48000, "macos": 256 / 48000}

# One page, every audio check. The rendered graph is the common fingerprint
# (triangle into a compressor); the silence and user-buffer checks are CreepJS's
# hasFakeAudio and its write/readback trap, which the old additive noise tripped.
AUDIO_JS = """
    (async () => {
      const oc = new OfflineAudioContext(1, 5000, 44100);
      const o = oc.createOscillator();
      o.type = 'triangle'; o.frequency.value = 10000;
      const c = oc.createDynamicsCompressor();
      c.threshold.value = -50; c.knee.value = 40; c.attack.value = 0;
      o.connect(c); c.connect(oc.destination); o.start(0);
      const b = await oc.startRendering();
      const data = b.getChannelData(0);
      const copy = new Float32Array(b.length);
      b.copyFromChannel(copy, 0);
      let sum = 0;
      for (const x of data) sum += Math.abs(x);

      const zc = new OfflineAudioContext(1, 100, 44100);
      const zo = zc.createOscillator();
      zo.frequency.value = 0; zo.start(0);
      const silence = [...new Set((await zc.startRendering()).getChannelData(0))];

      const v = Math.fround(0.123456789);
      const ub = new AudioBuffer({length: 2000, sampleRate: 44100});
      for (const i of [300, 310, 320]) ub.getChannelData(0)[i] = v;
      const ucopy = new Float32Array(2000);
      ub.copyFromChannel(ucopy, 0);
      const written = [...ub.getChannelData(0)].map((x, i) => [300, 310, 320].includes(i) ? x === v : x === 0);
      const copied = [...ucopy].map((x, i) => x === ub.getChannelData(0)[i]);
      const ub2 = new AudioBuffer({length: 2000, sampleRate: 44100});
      ub2.copyToChannel(new Float32Array(2000).fill(v), 0);

      const ac = new AudioContext();
      const hw = new AudioContext({renderSizeHint: 'hardware'});
      const rt = {
        sampleRate: ac.sampleRate, baseLatency: ac.baseLatency, renderQuantumSize: ac.renderQuantumSize,
        hwRenderQuantumSize: hw.renderQuantumSize, hwBaseLatency: hw.baseLatency,
      };
      await ac.close(); await hw.close();
      return {
        sum, readPathsAgree: data.every((x, i) => Object.is(x, copy[i])),
        silence, userBufferIntact: written.every(Boolean) && copied.every(Boolean) &&
          ub2.getChannelData(0).every((x) => x === v),
        ...rt,
      };
    })()
"""


def _json_dict(raw: str) -> dict:
    try:
        value = json.loads(raw)
    except ValueError:
        return {}
    return value if isinstance(value, dict) else {}


def audio_checks(profile_args: list[str]) -> None:
    print("\n=== Audio: seeded offline scale and realtime device (patches #26, #61) ===")
    runs = {}
    for label, extra in (("seed-a", ()), ("seed-b", ()), ("noise-off", ("--fingerprint-noise=false",))):
        with launch(*profile_args, *extra):
            time.sleep(0.5)
            runs[label] = cdp_eval(AUDIO_JS)
            print(f"  {label}: {runs[label]}")
    a, b, off = (_json_dict(runs[k]) for k in ("seed-a", "seed-b", "noise-off"))
    expect("offline sum stable for one seed across launches", f"{a.get('sum')} / {b.get('sum')}",
           lambda _: isinstance(a.get("sum"), float) and a.get("sum") == b.get("sum"),
           "identical sums")
    stock = off.get("sum")
    expect("offline sum scaled, and only slightly", f"{a.get('sum')} vs noise-off {stock}",
           lambda _: isinstance(stock, float) and stock > 0 and isinstance(a.get("sum"), float) and
           0 < abs(a["sum"] - stock) / stock < 5e-7,
           "differs from the unscaled sum by a relative 0 < d < 5e-7")
    expect("getChannelData == copyFromChannel", runs["seed-a"],
           lambda v: json_ok(v, lambda r: r.get("readPathsAgree") is True), "identical samples")
    expect("zero oscillator renders all zero (CreepJS hasFakeAudio)", runs["seed-a"],
           lambda v: json_ok(v, lambda r: r.get("silence") == [0]), "silence == [0]")
    expect("user-built AudioBuffer untouched (CreepJS trap)", runs["seed-a"],
           lambda v: json_ok(v, lambda r: r.get("userBufferIntact") is True),
           "written samples read back exactly, zeros stay zero")
    want = AUDIO_BASE_LATENCY[SMOKE_PROFILE]
    expect("AudioContext sampleRate/baseLatency", runs["seed-a"],
           lambda v: json_ok(v, lambda r: r.get("sampleRate") == 48000 and
                             abs(r.get("baseLatency", 0) - want) < 1e-9),
           f"48000 Hz, baseLatency {want}")
    # Real Chrome 154 answers renderSizeHint "hardware" with 128, not the
    # device buffer: Blink resolves the hint to the default quantum.
    expect("renderQuantumSize default and hardware hint", runs["seed-a"],
           lambda v: json_ok(v, lambda r: r.get("renderQuantumSize") == 128 and
                             r.get("hwRenderQuantumSize") == 128 and
                             abs(r.get("hwBaseLatency", 0) - want) < 1e-9),
           f"128 for both, hardware-hint baseLatency {want}")


# Patch #55. One fixed 2D canvas read back through every canvas API. A probe
# that reads the same canvas twice must see the same bytes: Bromite's per-call
# RNG failed exactly that, and a detector only has to compare two reads. The
# decoded-pixel fallback covers the one-shot and streaming PNG encoders ever
# emitting different bytes for the same pixels.
CANVAS_JS = """
    (async () => {
      const fnv = (s) => {
        let h = 0x811c9dc5;
        for (let i = 0; i < s.length; i++) {
          h ^= typeof s === 'string' ? s.charCodeAt(i) : s[i];
          h = Math.imul(h, 0x01000193) >>> 0;
        }
        return h.toString(16);
      };
      const draw = (ctx) => {
        ctx.fillStyle = '#f60'; ctx.fillRect(125, 1, 62, 20);
        ctx.fillStyle = '#069'; ctx.font = '11pt serif';
        ctx.fillText('Cwm fjordbank glyphs vext quiz, \\u{1F603}', 2, 15);
        ctx.fillStyle = 'rgba(102, 204, 0, 0.7)'; ctx.font = '18pt sans-serif';
        ctx.fillText('Cwm fjordbank glyphs vext quiz, \\u{1F603}', 4, 45);
      };
      const c = document.createElement('canvas');
      c.width = 240; c.height = 60;
      const ctx = c.getContext('2d');
      draw(ctx);
      const urls = [0, 1, 2].map(() => c.toDataURL());
      const imgs = [0, 1, 2].map(() => fnv(ctx.getImageData(0, 0, 240, 60).data));
      ctx.font = '18pt sans-serif';
      const texts = [0, 1].map(() => ctx.measureText('Cwm fjordbank glyphs vext quiz').width);

      const asDataURL = (blob) => new Promise((res) => {
        const r = new FileReader(); r.onload = () => res(r.result); r.readAsDataURL(blob);
      });
      const pixels = async (url) => {
        const im = new Image(); im.src = url; await im.decode();
        const k = document.createElement('canvas');
        k.width = im.width; k.height = im.height;
        const kx = k.getContext('2d'); kx.drawImage(im, 0, 0);
        return fnv(kx.getImageData(0, 0, k.width, k.height).data);
      };
      const blob = await asDataURL(await new Promise((res) => c.toBlob(res)));
      const off = new OffscreenCanvas(240, 60);
      off.getContext('2d').drawImage(c, 0, 0);
      const converted = await asDataURL(await off.convertToBlob());
      const same = async (url) => url === urls[0] || (await pixels(url)) === (await pixels(urls[0]));

      const cleared = document.createElement('canvas');
      cleared.width = 240; cleared.height = 60;
      const cx = cleared.getContext('2d');
      draw(cx);
      cx.clearRect(0, 0, 240, 60);
      const clearMax = Math.max(...cx.getImageData(0, 0, 8, 8).data);
      const clearFullMax = Math.max(...cx.getImageData(0, 0, 240, 60).data);

      const m = document.createElement('canvas').getContext('2d').measureText('');
      return {
        urls: urls.map(fnv), imgs, texts,
        toBlobSame: await same(blob), convertToBlobSame: await same(converted),
        toBlobBytes: blob === urls[0], convertToBlobBytes: converted === urls[0],
        clearMax, clearFullMax,
        empty: [m.width, m.actualBoundingBoxLeft, m.actualBoundingBoxRight,
                m.actualBoundingBoxAscent, m.actualBoundingBoxDescent,
                m.fontBoundingBoxAscent, m.fontBoundingBoxDescent],
      };
    })()
"""


def canvas_noise_checks() -> None:
    """Patch #55: canvas noise is a pure function of the seed, and measureText -
    whose noise production leaves off - reads exact, grid-aligned widths."""
    print("\n=== Canvas noise: stable per seed (patch 0055) ===")
    noise_flags = ("--fingerprinting-canvas-image-data-noise",)
    runs: dict[str, dict] = {}
    for label, seed in (("42069", "42069"), ("42069 relaunch", "42069"), ("1", "1")):
        seed_args, _ = _font_profile_args(seed)
        with launch(*seed_args, *noise_flags):
            time.sleep(0.5)
            out = cdp_eval(CANVAS_JS)
            print(f"  seed={label} {out}")
            try:
                runs[label] = json.loads(out)
            except ValueError:
                runs[label] = {}

    def first(run: dict, keys=("urls", "imgs", "texts")) -> tuple:
        return tuple((run.get(k) or [None])[0] for k in keys)

    a, again, other = runs["42069"], runs["42069 relaunch"], runs["1"]
    expect("(a) canvas reads repeat within a page",
           json.dumps({k: a.get(k) for k in ("urls", "imgs", "texts")}),
           lambda _: all(len(set(a.get(k) or [None, 0])) == 1 for k in ("urls", "imgs", "texts")),
           "3x toDataURL, 3x getImageData and 2x measureText identical")
    expect("(a) canvas reads repeat after a relaunch", json.dumps([first(a), first(again)]),
           lambda _: None not in first(a) and first(a) == first(again),
           "same toDataURL, getImageData and measureText for the same seed")
    pixels = ("urls", "imgs")
    expect("(b) canvas pixels differ across seeds",
           json.dumps([first(a, pixels), first(other, pixels)]),
           lambda _: None not in first(other, pixels) and
           all(x != y for x, y in zip(first(a, pixels), first(other, pixels))),
           "seed 1 and 42069 differ on toDataURL and getImageData")
    # Real widths land on a 1/4096 grid (HarfBuzz advances summed in fixed point);
    # measureText noise scaled them off it, a one-line tell, so it stays off.
    widths = [first(a, ("texts",))[0], first(other, ("texts",))[0]]
    expect("(b) measureText is exact: seed-independent and grid-aligned", json.dumps(widths),
           lambda _: isinstance(widths[0], (int, float)) and widths[0] == widths[1] and
           float(widths[0] * 4096).is_integer(),
           "seed 1 and 42069 measure the same width, a multiple of 1/4096")
    expect("(c) cleared canvas reads back all zero",
           json.dumps({k: a.get(k) for k in ("clearMax", "clearFullMax")}),
           lambda _: a.get("clearMax") == 0 and a.get("clearFullMax") == 0,
           "max byte 0 for the 8x8 and the full read after clearRect")
    expect("(d) measureText('') values are integers", json.dumps(a.get("empty")),
           lambda _: bool(a.get("empty")) and
           all(isinstance(v, (int, float)) and float(v).is_integer() for v in a["empty"]),
           "width, actual and font bounding boxes all integral")
    expect("(e) toBlob and convertToBlob match toDataURL",
           json.dumps({k: a.get(k) for k in (
               "toBlobSame", "convertToBlobSame", "toBlobBytes", "convertToBlobBytes")}),
           lambda _: a.get("toBlobSame") is True and a.get("convertToBlobSame") is True,
           "same bytes, or failing that the same decoded pixels")


# --- Patch 0057: WebRTC candidates at --fingerprint-webrtc-ip -------------------
WEBRTC_FORCED_IP = "203.0.113.7"


def _webrtc_gather_js(ice_url: str) -> str:
    """Two peer connections in turn: Chrome's mDNS responder keeps one name per
    address, so both must report the same .local host. 4 s each keeps both
    inside cdp_eval's 10 s socket timeout."""
    return """
    (async () => {
      const gather = () => new Promise(res => {
        const out = [];
        const pc = new RTCPeerConnection({iceServers: [{urls: %s}]});
        const done = () => { try { pc.close(); } catch (e) {} res(out); };
        pc.onicecandidate = e => {
          if (!e.candidate) return done();
          const c = e.candidate;
          if (c.candidate) out.push({type: c.type, protocol: c.protocol,
            address: c.address, relatedAddress: c.relatedAddress,
            relatedPort: c.relatedPort});
        };
        pc.createDataChannel("probe");
        pc.createOffer().then(o => pc.setLocalDescription(o));
        setTimeout(done, 4000);
      });
      return [await gather(), await gather()];
    })()
    """ % json.dumps(ice_url)


@contextmanager
def udp_listener() -> Iterator[tuple[str, list]]:
    """A local STUN "server" that only counts what reaches it."""
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind(("127.0.0.1", 0))
    sock.settimeout(0.2)
    packets: list = []
    stop = threading.Event()

    def run() -> None:
        while not stop.is_set():
            try:
                packets.append(sock.recvfrom(2048)[1])
            except socket.timeout:
                continue
            except OSError:
                return

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    try:
        yield f"stun:127.0.0.1:{sock.getsockname()[1]}", packets
    finally:
        stop.set()
        thread.join()
        sock.close()


def webrtc_checks(profile_args: list[str]) -> None:
    print("\n=== WebRTC forced IP (patch 0057) ===")
    with udp_listener() as (ice_url, packets), \
            launch(*profile_args, f"--fingerprint-webrtc-ip={WEBRTC_FORCED_IP}"):
        time.sleep(0.5)
        out = cdp_eval(_webrtc_gather_js(ice_url))
        time.sleep(1)
        print(f"  candidates: {out}")

        def shape(runs: list) -> bool:
            names = []
            for cands in runs:
                hosts = [c for c in cands if c["type"] == "host"]
                srflx = [c for c in cands if c["type"] == "srflx"]
                if not hosts or not all(c["address"].endswith(".local") for c in hosts):
                    return False
                if not srflx or {c["address"] for c in srflx} != {WEBRTC_FORCED_IP}:
                    return False
                if any((c["relatedAddress"], c["relatedPort"]) != ("0.0.0.0", 0) for c in srflx):
                    return False
                if any(c["type"] not in ("host", "srflx") for c in cands):
                    return False
                names.append(sorted(c["address"] for c in hosts))
            return len(runs) == 2 and names[0] == names[1]

        expect("candidates: .local host + srflx at the forced IP", out,
               lambda v: json_ok(v, shape),
               f"host <uuid>.local (same across two connections), srflx "
               f"{WEBRTC_FORCED_IP} raddr 0.0.0.0:0, nothing else")
        expect("UDP packets reaching the ICE server (forced)", str(len(packets)),
               lambda v: v == "0", "0 - the srflx is fabricated, nothing is sent")

    # Without the switch ungoogled's disable_non_proxied_udp default must still
    # hold, which is what the daemon relies on when no exit IP resolves.
    with udp_listener() as (ice_url, packets), launch(*profile_args):
        time.sleep(0.5)
        out = cdp_eval(_webrtc_gather_js(ice_url))
        time.sleep(1)
        expect("candidates without --fingerprint-webrtc-ip", out,
               lambda v: json_ok(v, lambda r: r == [[], []]), "none (fail closed)")
        expect("UDP packets reaching the ICE server (no switch)", str(len(packets)),
               lambda v: v == "0", "0")


# Driver-shaped: every driver enables the Runtime domain, and from then on V8
# previews each console argument, firing page getters a debugger-free browser
# never fires (issue #50, patch 0056). The rest of this gate evaluates with
# Runtime off, so it cannot see this class at all. Each count is how often a
# page getter fired around one console call; with Runtime enabled - on the page
# and, through auto-attach, in a worker, the way playwright attaches - every
# count must equal the debugger-free run. Real Chrome reads errorName 1,
# regexpFlag 1, tableColumn 1, nodeListLength 0; unpatched, each Runtime-enabled
# session adds one more (errorName also +1 for formatting the stack).
# benches/probes.py has the same probe; the build container mounts only validate/.
CDP_GETTER_PROBE = r"""async () => {
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
}"""


def driver_shaped_getter_reads() -> tuple[dict, dict]:
    """The probe with Runtime off, then again with Runtime on as a driver has it."""
    with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/list", timeout=5) as r:
        page = next(t for t in json.loads(r.read()) if t.get("type") == "page")
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=15)
    state = {"id": 0}

    def call(method: str, params: dict | None = None, session: str | None = None) -> dict:
        msg_id = _next_id(state)
        msg: dict = {"id": msg_id, "method": method, "params": params or {}}
        if session:
            msg["sessionId"] = session
        ws.send(json.dumps(msg))
        if session:
            return {}
        while True:
            msg = json.loads(ws.recv())
            if msg.get("method") == "Target.attachedToTarget":
                child = msg["params"]["sessionId"]
                call("Runtime.enable", session=child)
                call("Runtime.runIfWaitingForDebugger", session=child)
            elif msg.get("id") == msg_id and "sessionId" not in msg:
                return msg

    def probe() -> dict:
        r = call("Runtime.evaluate", {"expression": f"({CDP_GETTER_PROBE})()",
                                      "returnByValue": True, "awaitPromise": True})
        return r.get("result", {}).get("result", {}).get("value") or {"error": r}

    try:
        before = probe()
        call("Target.setAutoAttach", {"autoAttach": True, "waitForDebuggerOnStart": True,
                                      "flatten": True})
        call("Runtime.enable")
        return before, probe()
    finally:
        ws.close()


# --- Display persona: patches 0011, 0013, 0054, 0062, 0064 ------------------
# Expected values are real Chrome 154's, measured on a MacBook Pro (light and
# dark) and a Windows 11 PC (light; dark Highlight derived from
# layout_theme_win.cc). The macOS smoke screen is 1710x1112, a notched MacBook
# Air 15", whose menu bar cuttle::seed::MenuBarHeight() puts at 38.
_ARIAL = ["Arial", "16px"]
_SEGOE = ['"Segoe UI"', "12px"]
DISPLAY_EXPECT = {
    "windows": {
        "colorDepth": 24, "availTop": 0, "colorBits": 8,
        "p3": False, "hdr": False,
        "light": {
            "ActiveText": "rgb(0, 102, 204)", "LinkText": "rgb(0, 102, 204)",
            "VisitedText": "rgb(0, 102, 204)", "ButtonFace": "rgb(240, 240, 240)",
            "ThreeDFace": "rgb(240, 240, 240)", "GrayText": "rgb(109, 109, 109)",
            "Highlight": "rgb(0, 120, 212)", "HighlightText": "rgb(255, 255, 255)",
            "SelectedItem": "rgb(25, 103, 210)", "SelectedItemText": "rgb(255, 255, 255)",
            "InactiveCaptionText": "rgb(128, 128, 128)",
        },
        # Windows keeps the stock palette in dark mode, but never blends Highlight.
        "dark": {"ActiveText": "rgb(255, 0, 0)", "Highlight": "rgb(25, 103, 210)"},
        "fonts": {"caption": _ARIAL, "icon": _ARIAL, "menu": _SEGOE,
                  "message-box": _ARIAL, "small-caption": _SEGOE, "status-bar": _SEGOE},
    },
    "macos": {
        "colorDepth": 30, "availTop": 38, "colorBits": 10,
        "p3": True, "hdr": True,
        # Real Chrome on a Mac keeps the stock red ActiveText: CreepJS's
        # hasKnownBgColor fires there too, so the persona must not "fix" it.
        "light": {
            "ActiveText": "rgb(255, 0, 0)", "ButtonFace": "rgb(239, 239, 239)",
            "Highlight": "rgba(128, 188, 254, 0.6)", "HighlightText": "rgb(0, 0, 0)",
            "SelectedItem": "rgb(179, 215, 255)", "SelectedItemText": "rgb(0, 0, 0)",
        },
        "dark": {
            "ActiveText": "rgb(255, 0, 0)", "Highlight": "rgba(179, 215, 255, 0.8)",
            "HighlightText": "rgb(0, 0, 0)", "SelectedItem": "rgb(153, 200, 255)",
            "SelectedItemText": "rgb(59, 59, 59)",
        },
        "fonts": {k: _ARIAL for k in ("caption", "icon", "menu", "message-box",
                                      "small-caption", "status-bar")},
    },
}
# V8 154's 64-bit heap_size_limit on any host with 8 GiB or more (patch 0064).
# Read on the trusted http page, whose site-locked renderer reports it precise,
# as real Chrome does. Non-vacuous only on a host under 8 GiB.
PERSONA_HEAP_LIMIT = 4395630592


def _cdp_send(method: str, params: dict) -> None:
    with urllib.request.urlopen(f"http://127.0.0.1:{PORT}/json/list", timeout=5) as r:
        targets = json.loads(r.read())
    page = next(t for t in targets if t.get("type") == "page")
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=10)
    try:
        ws.send(json.dumps({"id": 1, "method": method, "params": params}))
        while json.loads(ws.recv()).get("id") != 1:
            pass
    finally:
        ws.close()


def display_persona_checks() -> None:
    want = DISPLAY_EXPECT[SMOKE_PROFILE]
    display = cdp_eval("""
        (() => {
          const mq = q => matchMedia(q).matches;
          let colorBits = null;
          for (let i = 0; i <= 16; i++) if (mq(`(color: ${i})`)) colorBits = i;
          return {
            availTop: screen.availTop, availLeft: screen.availLeft,
            availHeight: screen.availHeight, height: screen.height,
            colorDepth: screen.colorDepth, colorBits,
            srgb: mq('(color-gamut: srgb)'), p3: mq('(color-gamut: p3)'),
            rec2020: mq('(color-gamut: rec2020)'),
            hdr: mq('(dynamic-range: high)'),
          };
        })()
    """)
    expect("display persona (availTop, depth, gamut, HDR)", display,
           lambda v: json_ok(v, lambda d:
               d.get("availTop") == want["availTop"] and d.get("availLeft") == 0 and
               d["availTop"] + d["availHeight"] <= d["height"] and
               d.get("colorDepth") == want["colorDepth"] and
               d.get("colorBits") == want["colorBits"] and
               d.get("srgb") is True and d.get("p3") is want["p3"] and
               d.get("rec2020") is False and d.get("hdr") is want["hdr"]),
           f"availTop {want['availTop']} inside the screen, colorDepth "
           f"{want['colorDepth']}, (color: {want['colorBits']}), p3 {want['p3']}, "
           f"rec2020 False, dynamic-range high {want['hdr']}")

    # Patch 0013: the window is the maximized one outer* describes, so it sits
    # at the work area's origin, and a real mouse event's screen coordinates
    # agree with it (measured on real Chrome 154 on a Mac and on Windows).
    cdp_eval("""
        window.__cuttleEvent = null;
        addEventListener('mousedown', e => { window.__cuttleEvent = {
          screenX: e.screenX, screenY: e.screenY, clientX: e.clientX, clientY: e.clientY}; },
          {once: true});
        true
    """)
    for kind in ("mousePressed", "mouseReleased"):
        _cdp_send("Input.dispatchMouseEvent",
                  {"type": kind, "x": 50, "y": 60, "button": "left", "clickCount": 1})
    window_state = cdp_eval("""
        ({screenX, screenY, screenLeft, screenTop, outerHeight, innerHeight,
          availLeft: screen.availLeft, availTop: screen.availTop,
          availHeight: screen.availHeight, event: window.__cuttleEvent})
    """)
    expect("window position (screenX/Y, event coordinates)", window_state,
           lambda v: json_ok(v, lambda w:
               w["screenX"] == w["availLeft"] == 0 and
               w["screenY"] == w["availTop"] == want["availTop"] and
               w["screenLeft"] == w["screenX"] and w["screenTop"] == w["screenY"] and
               w["screenY"] + w["outerHeight"] <= w["availTop"] + w["availHeight"] and
               w["event"]["screenX"] - w["event"]["clientX"] == w["screenX"] and
               w["event"]["screenY"] - w["event"]["clientY"] ==
               w["screenY"] + w["outerHeight"] - w["innerHeight"]),
           f"screenX 0, screenY {want['availTop']} (availTop), inside the work "
           "area; event.screen - event.client = window.screen + chrome")

    names = json.dumps(sorted({*want["light"], *want["dark"]}))
    colors = cdp_eval(f"""
        (() => {{
          const d = document.createElement('div');
          document.body.appendChild(d);
          const out = {{light: {{}}, dark: {{}}}};
          for (const scheme of ['light', 'dark']) for (const k of {names}) {{
            d.setAttribute('style', `background-color: ${{k}}; color-scheme: ${{scheme}}`);
            out[scheme][k] = getComputedStyle(d).backgroundColor;
          }}
          d.remove();
          return out;
        }})()
    """)
    expect("CSS system colours (light and dark)", colors,
           lambda v: json_ok(v, lambda c: all(
               c[scheme].get(k) == val
               for scheme in ("light", "dark") for k, val in want[scheme].items())),
           f"light {want['light']}, dark {want['dark']}")

    fonts = cdp_eval(f"""
        (() => {{
          const d = document.createElement('div');
          document.body.appendChild(d);
          const out = {{}};
          for (const k of {json.dumps(sorted(want["fonts"]))}) {{
            d.setAttribute('style', `font: ${{k}}`);
            const s = getComputedStyle(d);
            out[k] = [s.fontFamily, s.fontSize];
          }}
          d.remove();
          return out;
        }})()
    """)
    expect("CSS system fonts", fonts,
           lambda v: json_ok(v, lambda f: f == want["fonts"]), str(want["fonts"]))

    expect("jsHeapSizeLimit", cdp_eval("performance.memory.jsHeapSizeLimit"),
           lambda v: v == str(PERSONA_HEAP_LIMIT), str(PERSONA_HEAP_LIMIT))


class EchoUserAgentHandler(BaseHTTPRequestHandler):
    """Serves a page carrying the User-Agent header its own request arrived with."""

    def do_GET(self) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        header = escape(self.headers.get("User-Agent", ""))
        self.wfile.write(f"<!doctype html><title>ua</title><pre id=ua>{header}</pre>".encode())

    def log_message(self, format: str, *args: object) -> None:
        return


UA_VERSION_JS = """
    (async () => {
      const worker = await new Promise(res => {
        const src = new Blob(["postMessage(navigator.userAgent)"], {type: "text/javascript"});
        const w = new Worker(URL.createObjectURL(src));
        w.onmessage = e => res(e.data);
        w.onerror = () => res(null);
      });
      return {
        page: navigator.userAgent, worker,
        header: document.getElementById("ua").textContent,
        brands: navigator.userAgentData ? navigator.userAgentData.brands : null,
      };
    })()
"""

# navigator.cpuPerformance by persona and core count, per upstream
# GetTierFromCpuInfo (content/browser/cpu_performance). Patch 0065 feeds it the
# persona's cores and, for an Apple GPU, the chip the WebGL renderer names. The
# macOS rows are the Apple M rule: 8-10 cores tier kUltra (4) where cores alone
# say kHigh (3). Real Chrome 154 on an M1 Max (10 cores) reports 4.
CPU_TIERS = {
    "windows": {4: 2, 8: 3, 12: 4},
    "macos": {8: 4, 10: 4},
}


def check_ua_version_and_cpu_tier(profile_args: list[str], profile: dict) -> None:
    """Patches 0006 and 0065: UA version agreement and the persona CPU tier.

    navigator.userAgent in the page and in a worker must equal the HTTP header
    byte for byte, and its Chrome/<major>.0.0.0 must be the major
    userAgentData.brands reports. 127.0.0.1 is a secure context, which
    cpuPerformance and userAgentData require.
    """
    print(f"\n=== UA version + cpuPerformance ({profile['label']} persona) ===")
    server = ThreadingHTTPServer(("127.0.0.1", 0), EchoUserAgentHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{server.server_address[1]}/"
    want_ua = f"Chrome/{CHROME_UA_VERSION} "

    def agrees(u: dict) -> bool:
        brands = {b["brand"]: b["version"] for b in u["brands"]}
        return (u["page"] == u["worker"] == u["header"] and want_ua in u["page"] and
                brands.get("Google Chrome") == brands.get("Chromium") == str(_MAJOR))

    try:
        for cores, tier in CPU_TIERS[SMOKE_PROFILE].items():
            with launch(*profile_args,
                        "--fingerprint-brand=Chrome",
                        f"--fingerprint-brand-version={CHROMIUM_VERSION}",
                        f"--fingerprint-hardware-concurrency={cores}"):
                time.sleep(0.5)
                cdp_navigate(url)
                time.sleep(1)
                out = cdp_eval(UA_VERSION_JS)
                got_tier = cdp_eval("navigator.cpuPerformance")
            expect(f"UA page == worker == header == brands ({cores} cores)", out,
                   lambda v: json_ok(v, agrees),
                   f"one UA everywhere carrying {want_ua.strip()}, brands at {_MAJOR}")
            expect(f"cpuPerformance ({cores} cores)", got_tier,
                   lambda v, t=tier: v == str(t), str(tier))
    finally:
        server.shutdown()
        server.server_close()


# --- WebGL/WebGPU persona capabilities (patches 0016, 0058, 0059, 0060) ---
# Expected values are real Chrome 154: the D3D11 row is the 46A6 Iris Xe from
# the Windows machine pool, the Metal row a real Apple M-series Mac.
GPU_CAPS_WINDOWS_RENDERER = (
    "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics (0x000046A6) Direct3D11 vs_5_0 ps_5_0, D3D11)"
)
GPU_CAPS_EXPECTED = {
    "windows": {
        "gl1": {"0x0D33": 16384, "0x0D3A": [32767, 32767], "0x846D": [1, 1024],
                "0x8DFB": 4096, "0x8B4D": 32, "0x8824": 8, "0x84FF": 16},
        "gl2": {"0x8D57": 16, "0x8A34": 256, "0x8A30": 65536, "0x8A31": 212992},
        "dialect": "hlsl",
        "head": "// INITIAL HLSL BEGIN\n\n#pragma warning( disable: 3081 3556 3557 3571 )\n",
        "hidden": ["WEBGL_compressed_texture_astc", "WEBGL_compressed_texture_etc",
                   "WEBGL_compressed_texture_etc1", "WEBGL_compressed_texture_pvrtc"],
    },
    "macos": {
        "gl1": {"0x0D33": 16384, "0x0D3A": [16384, 16384], "0x846D": [1, 511],
                "0x8DFB": 1024, "0x8B4D": 32, "0x8824": 8, "0x84FF": 16},
        "gl2": {"0x8D57": 4, "0x8A34": 16, "0x8A30": 16384, "0x8A31": 69632},
        "dialect": "msl",
        "head": "\n\n#include <metal_stdlib>\n",
        "hidden": [],
    },
}
# Real Chrome 154 on macOS and Windows expose exactly these.
REAL_WGSL_LANGUAGE_FEATURES = sorted([
    "buffer_view", "immediate_address_space", "linear_indexing",
    "packed_4x8_integer_dot_product", "pointer_composite_access",
    "readonly_and_readwrite_storage_textures", "subgroup_id", "subgroup_uniformity",
    "swizzle_assignment", "texture_and_sampler_let", "texture_formats_tier1",
    "uniform_buffer_standard_layout", "unrestricted_pointer_parameters",
])
GPU_CAPS_JS = r"""
(async () => {
  // deviceandbrowserinfo's classifier, verbatim.
  const lang = v => /register\(b\d+\)|cbuffer|dx_ViewAdjust/i.test(v) ? 'hlsl'
    : /metal_stdlib|\[\[stage_in\]\]|\[\[buffer\(/i.test(v) ? 'msl'
    : (/#version\s+\d/i.test(v) || /\bgl_Position\b|\battribute\b|\bvarying\b|\bgl_FragColor\b/.test(v))
      ? 'glsl' : 'unknown';
  const probeNames = ['WEBGL_compressed_texture_astc', 'WEBGL_compressed_texture_etc',
    'WEBGL_compressed_texture_etc1', 'WEBGL_compressed_texture_pvrtc',
    'EXT_color_buffer_float', 'WEBGL_debug_shaders', 'OES_texture_float'];
  const one = (kind, pnames) => {
    const gl = document.createElement('canvas').getContext(kind);
    if (!gl) return null;
    const exts = gl.getSupportedExtensions();
    const agree = probeNames.every(n => (gl.getExtension(n) !== null) === exts.includes(n));
    gl.getExtension('WEBGL_draw_buffers');
    gl.getExtension('EXT_texture_filter_anisotropic');
    const params = {};
    for (const p of pnames) {
      const v = gl.getParameter(p);
      params['0x' + p.toString(16).toUpperCase().padStart(4, '0')] =
        (v && typeof v === 'object' && 'length' in v) ? Array.from(v) : v;
    }
    const prec = [];
    for (const s of [gl.VERTEX_SHADER, gl.FRAGMENT_SHADER])
      for (const t of [gl.LOW_FLOAT, gl.MEDIUM_FLOAT, gl.HIGH_FLOAT, gl.LOW_INT, gl.MEDIUM_INT, gl.HIGH_INT]) {
        const f = gl.getShaderPrecisionFormat(s, t);
        prec.push([f.rangeMin, f.rangeMax, f.precision].join(','));
      }
    let shader = null;
    const dbg = gl.getExtension('WEBGL_debug_shaders');
    if (dbg) {
      const sh = gl.createShader(gl.VERTEX_SHADER);
      gl.shaderSource(sh, 'attribute vec4 p;void main(){gl_Position=p;}');
      gl.compileShader(sh);
      const v = dbg.getTranslatedShaderSource(sh) || '';
      const other = gl.createShader(gl.VERTEX_SHADER);
      gl.shaderSource(other, 'attribute vec4 q;void main(){gl_Position=q*2.0;}');
      gl.compileShader(other);
      shader = {lang: lang(v), src: v.slice(0, 120),
                otherNonEmpty: (dbg.getTranslatedShaderSource(other) || '').length > 0};
    }
    return {exts, agree, params, prec: [...new Set(prec)], shader};
  };
  const out = {
    gl1: one('webgl', [0x0D33, 0x0D3A, 0x846D, 0x8DFB, 0x8B4D, 0x8824, 0x84FF]),
    gl2: one('webgl2', [0x8D57, 0x8A34, 0x8A30, 0x8A31]),
    gpu: null,
  };
  if (navigator.gpu) {
    const a = await navigator.gpu.requestAdapter();
    const fb = await navigator.gpu.requestAdapter({forceFallbackAdapter: true});
    out.gpu = {
      format: navigator.gpu.getPreferredCanvasFormat(),
      wgsl: [...navigator.gpu.wgslLanguageFeatures].sort(),
      adapterFallback: a ? a.info.isFallbackAdapter : null,
      forcedFallback: fb ? fb.info.isFallbackAdapter : null,
    };
  }
  return out;
})()
"""


def gpu_caps_checks(profile_args: list[str]) -> None:
    print("\n=== WebGL/WebGPU persona capabilities ===")
    want = GPU_CAPS_EXPECTED[SMOKE_PROFILE]
    gpu_args = []
    if SMOKE_PROFILE == "windows":
        gpu_args = ["--fingerprint-gpu-vendor=Google Inc. (Intel)",
                    f"--fingerprint-gpu-renderer={GPU_CAPS_WINDOWS_RENDERER}"]
    with trusted_local_page() as (trusted_url, trusted_origin), \
            launch(*profile_args, *gpu_args,
                   f"--unsafely-treat-insecure-origin-as-secure={trusted_origin}"):
        time.sleep(0.5)
        cdp_navigate(trusted_url)
        time.sleep(0.5)
        raw = cdp_eval(GPU_CAPS_JS)
        for gl in ("gl1", "gl2"):
            expect(f"{gl} caps = real {SMOKE_PROFILE} GPU", raw,
                   lambda v, g=gl: json_ok(v, lambda d: d[g]["params"] == want[g]),
                   json.dumps(want[gl]))
            expect(f"{gl} precision = full 32-bit", raw,
                   lambda v, g=gl: json_ok(v, lambda d:
                       sorted(d[g]["prec"]) == ["127,127,23", "31,30,0"]),
                   "floats 127,127,23 and ints 31,30,0 at every precision")
            expect(f"{gl} getExtension agrees with getSupportedExtensions", raw,
                   lambda v, g=gl: json_ok(v, lambda d: d[g]["agree"] is True),
                   "a hidden extension is not gettable either")
            expect(f"{gl} hides extensions the persona GPU lacks", raw,
                   lambda v, g=gl: json_ok(v, lambda d:
                       not set(want["hidden"]) & set(d[g]["exts"])),
                   f"none of {want['hidden']}")
        expect("gl2 keeps WebGL2-only extensions", raw,
               lambda v: json_ok(v, lambda d: "EXT_color_buffer_float" in d["gl2"]["exts"]),
               "EXT_color_buffer_float listed")
        expect(f"translated shader = {want['dialect']}", raw,
               lambda v: json_ok(v, lambda d:
                   d["gl1"]["shader"]["lang"] == want["dialect"] and
                   d["gl1"]["shader"]["src"].startswith(want["head"])),
               f"{want['dialect']}, starting {want['head']!r}")
        expect("unknown shader keeps the backend translation", raw,
               lambda v: json_ok(v, lambda d: d["gl1"]["shader"]["otherNonEmpty"] is True),
               "non-empty")
        expect("WebGPU coherent with a hardware GPU", raw,
               lambda v: json_ok(v, lambda d: d["gpu"] is None or (
                   d["gpu"]["format"] == "bgra8unorm" and
                   d["gpu"]["wgsl"] == REAL_WGSL_LANGUAGE_FEATURES and
                   d["gpu"]["adapterFallback"] in (None, False) and
                   d["gpu"]["forcedFallback"] is None)),
               "bgra8unorm, real 154 WGSL features, no CPU fallback adapter")


# --- Patch #63: system-ui is the persona's system font ----------------------
# Real Chrome 154.0.8037.58 on macOS 27.2 measures SYSTEM_UI_TEXT in system-ui
# (SF Pro) at these widths; the pack's re-widthed stand-in lands within 0.05%.
SYSTEM_UI_TEXT = "The quick brown fox jumps over the lazy dog 0123456789"
MACOS_SYSTEM_UI_WIDTHS = {13: 351.622, 16: 420.953}
# Real Chrome 154 on Windows 11 measures system-ui and "Segoe UI" alike; the
# pack's Selawik-based "Segoe UI" carries Segoe UI's advances and matches exactly.
WINDOWS_SYSTEM_UI_WIDTHS = {13: 331.348, 16: 407.813}
# A kerning-heavy string at 16px: real Segoe UI kerns it 7% narrower than its
# advances, and the pack's face carries the same pairs.
WINDOWS_KERN_TEXT, WINDOWS_KERN_WIDTH = "AVAWAY To Ta Te Yo LT", 159.5
# The stand-in's internal family (ops/docker/Dockerfile). It must never resolve
# by name, in any spelling fontconfig would still match.
SYSTEM_UI_FACE = "sysui-q7k2"
# Absent on a real Mac: the internal face, and names real Chrome does not
# resolve (it falls back to the default font for all of them).
SYSTEM_UI_ABSENT = (SYSTEM_UI_FACE, "SYSUI Q7K2", "SF Pro", ".SF NS", "-apple-system")


def system_ui_checks() -> None:
    state = cdp_eval(f"""
        (async () => {{
          const S = {json.dumps(SYSTEM_UI_TEXT)};
          const ctx = document.createElement("canvas").getContext("2d");
          const width = (css) => {{ ctx.font = css; return ctx.measureText(S).width; }};
          const generics = ["serif", "sans-serif", "monospace"];
          const present = (family) => generics.some(
            (g) => Math.abs(width(`16px ${{family}}, ${{g}}`) - width(`16px ${{g}}`)) > 0.5);
          const local = async (name) => {{
            try {{ await new FontFace("t", `local(${{JSON.stringify(name)}})`).load(); return true; }}
            catch (e) {{ return false; }}
          }};
          const absent = {json.dumps(SYSTEM_UI_ABSENT)};
          return {{
            widths: {{13: width("13px system-ui"), 16: width("16px system-ui")}},
            segoe: {{13: width('13px "Segoe UI"'), 16: width('16px "Segoe UI"')}},
            kerned: (ctx.font = '16px "Segoe UI"', ctx.measureText({json.dumps(WINDOWS_KERN_TEXT)}).width),
            blink: width("16px BlinkMacSystemFont"),
            system: width("16px system-ui"),
            blinkPresent: present("BlinkMacSystemFont"),
            blinkLowerPresent: present("blinkmacsystemfont"),
            presentByName: Object.fromEntries(absent.map((f) => [f, present(JSON.stringify(f))])),
            localFace: await local({json.dumps(SYSTEM_UI_FACE)}),
            localControl: await local(navigator.platform === "MacIntel" ? "Helvetica" : "Arial"),
          }};
        }})()
    """)
    expect("system-ui face absent by name", state,
           lambda v: json_ok(v, lambda s: not any(s["presentByName"].values())),
           ", ".join(SYSTEM_UI_ABSENT) + " all fall back")
    expect("system-ui face absent from local()", state,
           lambda v: json_ok(v, lambda s: s["localFace"] is False),
           f"local({SYSTEM_UI_FACE!r}) rejects")
    if FONTS_DIR:
        # Without it the local() rejection above could be local() not working.
        expect("local() control resolves", state,
               lambda v: json_ok(v, lambda s: s["localControl"] is True),
               "a pack font loads through local()")
    if SMOKE_PROFILE == "windows":
        expect("BlinkMacSystemFont absent (Windows)", state,
               lambda v: json_ok(v, lambda s: s["blinkPresent"] is False),
               "unresolved, as on real Windows Chrome")
        if not FONTS_DIR:
            print("  [SKIP] system-ui width - BROWSER_FONTS_DIR unset (no Segoe UI)")
            return
        for label, key in (("system-ui", "widths"), ('"Segoe UI"', "segoe")):
            expect(f"{label} measures like Segoe UI (Windows)", state,
                   lambda v, key=key: json_ok(v, lambda s: all(
                       abs(s[key][str(px)] - want) / want < 0.005
                       for px, want in WINDOWS_SYSTEM_UI_WIDTHS.items())),
                   ", ".join(f"{want} at {px}px" for px, want in WINDOWS_SYSTEM_UI_WIDTHS.items())
                   + " (+/-0.5%)")
        expect('"Segoe UI" kerns like Segoe UI (Windows)', state,
               lambda v: json_ok(v, lambda s: abs(s["kerned"] - WINDOWS_KERN_WIDTH) / WINDOWS_KERN_WIDTH < 0.005),
               f"{WINDOWS_KERN_TEXT!r} {WINDOWS_KERN_WIDTH} at 16px (+/-0.5%)")
        return
    expect("BlinkMacSystemFont = system-ui (macOS)", state,
           lambda v: json_ok(v, lambda s: abs(s["blink"] - s["system"]) < 0.01
                             and s["blinkLowerPresent"] is False),
           "same width as system-ui; lowercase spelling unresolved")
    if not FONTS_DIR:
        print("  [SKIP] system-ui width - BROWSER_FONTS_DIR unset (no pack, no stand-in)")
        return
    expect("system-ui measures like SF Pro (macOS)", state,
           lambda v: json_ok(v, lambda s: all(
               abs(s["widths"][str(px)] - want) / want < 0.005
               for px, want in MACOS_SYSTEM_UI_WIDTHS.items())),
           ", ".join(f"{want} at {px}px" for px, want in MACOS_SYSTEM_UI_WIDTHS.items())
           + " (+/-0.5%)")


def main() -> int:
    seed = "42069"
    profile_args, profile = _font_profile_args(seed)
    args = [
        *profile_args,
        "--fingerprint-brand=Chrome",
        f"--fingerprint-brand-version={CHROMIUM_VERSION}",
        "--fingerprint-hardware-concurrency=12",
        f"--fingerprint-device-memory={profile['device_memory']}",
        f"--fingerprint-screen-width={profile['screen'][0]}",
        f"--fingerprint-screen-height={profile['screen'][1]}",
        f"--fingerprint-taskbar-height={profile['screen'][2]}",
        # NOT --fingerprint-max-touch-points: production never sends it, so
        # supplying it here would assert a value we do not ship. The
        # maxTouchPoints assertion below reads the binary's own default.
        "--fingerprint-timezone=America/New_York",
        "--fingerprint-locale=en-US",
        "--fingerprint-network-profile=datacenter",
        "--accept-lang=en-US,en",
        # The five flags below are emitted by production (DefaultStealthArgs /
        # ForkParityArgs / ScreenArgs) and were absent from this gate, so every
        # assertion above ran against a browser we do not ship. That is how the
        # BarcodeDetector assertion first came up "present": false on a binary
        # that implements it correctly. TestSmokeMatchesProductionFlags pins
        # the key set so this cannot silently recur.
        f"--enable-blink-features={profile['blink_features']}",
        "--enable-features=WebBluetooth",
        "--blink-settings=availablePointerTypes=4,availableHoverTypes=2,"
        "primaryPointerType=4,primaryHoverType=2,preferredColorScheme=0",
        "--use-fake-device-for-media-stream",
        f"--window-size={profile['screen'][0]},{profile['screen'][1] - profile['screen'][2]}",
        "--fingerprinting-canvas-image-data-noise",
    ]

    print(f"=== JS-surface vectors ({profile['label']} persona) ===")
    with trusted_local_page() as (trusted_url, trusted_origin), \
            launch(*args, f"--unsafely-treat-insecure-origin-as-secure={trusted_origin}"):
        time.sleep(0.5)
        expect("navigator.webdriver", cdp_eval("navigator.webdriver"), lambda v: v == "false", "false")
        expect("navigator.plugins.length", cdp_eval("navigator.plugins.length"), lambda v: v == "5", "5")
        expect("typeof window.chrome", cdp_eval("typeof window.chrome"), lambda v: v == '"object"', '"object"')
        expect("navigator.platform", cdp_eval("navigator.platform"),
               lambda v: v == json.dumps(profile["navigator_platform"]),
               json.dumps(profile["navigator_platform"]))
        expect("hardwareConcurrency", cdp_eval("navigator.hardwareConcurrency"), lambda v: v == "12", "12")
        expect("maxTouchPoints", cdp_eval("navigator.maxTouchPoints"), lambda v: v == "0", "0")
        screen_state = cdp_eval("""
            ({
              width: screen.width, height: screen.height,
              availWidth: screen.availWidth, availHeight: screen.availHeight,
              colorDepth: screen.colorDepth, pixelDepth: screen.pixelDepth,
              outerWidth: window.outerWidth, outerHeight: window.outerHeight,
              devicePixelRatio: window.devicePixelRatio,
            })
        """)
        want_w, want_h, want_bar = profile["screen"]
        expect("screen/window coherent", screen_state,
               lambda v: json_ok(v, lambda s:
                   isinstance(s, dict) and
                   s.get("width") == want_w and s.get("height") == want_h and
                   s.get("availWidth") == s.get("width") and
                   s.get("height", 0) - s.get("availHeight", 0) == want_bar and
                   s.get("outerWidth") == s.get("width") and
                   s.get("outerHeight") == s.get("availHeight") and
                   s.get("colorDepth") == DISPLAY_EXPECT[SMOKE_PROFILE]["colorDepth"] and
                   s.get("pixelDepth") == s.get("colorDepth") and
                   s.get("devicePixelRatio") == profile["dpr"]),
               f"screen {want_w}x{want_h}, taskbar {want_bar}, matching outer "
               f"size, {DISPLAY_EXPECT[SMOKE_PROFILE]['colorDepth']}-bit depth, "
               f"DPR {profile['dpr']}")

        # Patch #54. CreepJS runs exactly these two queries and reports
        # "Screen: failed matchMedia" / "Window.devicePixelRatio: lied dpr"
        # when they disagree with screen.* and devicePixelRatio. Non-vacuous by
        # construction: both sides are read from the binary, neither is
        # supplied by this gate, so it can only pass if the spoofed values and
        # CSS media evaluation resolve from the same source.
        media_state = cdp_eval("""
            ({
              mmDevice: matchMedia(
                `(device-width: ${screen.width}px) and (device-height: ${screen.height}px)`
              ).matches,
              mmRes: matchMedia(`(resolution: ${window.devicePixelRatio}dppx)`).matches,
            })
        """)
        expect("media queries agree with screen/DPR", media_state,
               lambda v: json_ok(v, lambda s:
                   isinstance(s, dict) and
                   s.get("mmDevice") is True and s.get("mmRes") is True),
               "device-width/height and resolution all match")
        # --blink-settings. preferredColorScheme=0 is kDark: cuttle's deliberate
        # default, and it also clears CreepJS's prefersLightColor likeHeadless
        # hit. The pointer/hover pair (availablePointerTypes=4 = fine,
        # availableHoverTypes=2 = hover) is what a real desktop mouse reports;
        # headless defaults to none/none, which is a direct tell.
        css_env = cdp_eval("""
            ({
              dark: matchMedia('(prefers-color-scheme: dark)').matches,
              pointerFine: matchMedia('(pointer: fine)').matches,
              anyPointerFine: matchMedia('(any-pointer: fine)').matches,
              hover: matchMedia('(hover: hover)').matches,
              anyHover: matchMedia('(any-hover: hover)').matches,
            })
        """)
        expect("color scheme + pointer/hover", css_env,
               lambda v: json_ok(v, lambda c:
                   isinstance(c, dict) and all(c.get(k) is True for k in
                   ("dark", "pointerFine", "anyPointerFine", "hover", "anyHover"))),
               "dark scheme, fine pointer, hover capable")

        expect("timezone", cdp_eval("Intl.DateTimeFormat().resolvedOptions().timeZone"),
               lambda v: v == '"America/New_York"', '"America/New_York"')
        expect("locale", cdp_eval("navigator.language"), lambda v: v == '"en-US"', '"en-US"')
        expect("Notification.permission", cdp_eval("Notification.permission"),
               lambda v: v == '"default"', '"default"')
        expect("permissions.query notifications", cdp_eval("""
            (async () => (await navigator.permissions.query({name: 'notifications'})).state)()
        """), lambda v: v == '"prompt"', '"prompt"')
        # Font presence by ADVANCE WIDTH, the way a detector actually probes it.
        # document.fonts.check() is not a font detector: per spec it reports
        # whether the *specified* font is loaded, and an unknown local family
        # resolves to fallback and counts as loaded - it returns true for
        # everything, including SENTINEL_FONT.
        #
        # Each family is measured against all three CSS generics. A family that
        # fell back is byte-identical to its generic; a family that happens to
        # match ONE generic (our Monaco has monospace's advance) still separates
        # from the other two, so requiring a difference against any one generic
        # avoids that false negative.
        probe_families = sorted({
            *WINDOWS_CORE_FONTS, *MACOS_CORE_FONTS,
            *SUBSTITUTE_SOURCE_FONTS, SENTINEL_FONT,
        })
        font_state = cdp_eval(f"""
            (() => {{
              const families = {json.dumps(probe_families)};
              const S = "mmmwwwiiilll0123456789WWMMil@#";
              const ctx = document.createElement("canvas").getContext("2d");
              const width = (css) => {{ ctx.font = `72px ${{css}}`; return ctx.measureText(S).width; }};
              const generics = ["serif", "sans-serif", "monospace"];
              const base = Object.fromEntries(generics.map((g) => [g, width(g)]));
              const present = {{}};
              for (const family of families) {{
                present[family] = generics.some(
                  (g) => Math.abs(width(`"${{family}}", ${{g}}`) - base[g]) > 0.5);
              }}
              return present;
            }})()
        """)
        expect("font probe self-check (sentinel must be absent)", font_state,
               lambda v: json_ok(v, lambda f: f.get(SENTINEL_FONT) is False),
               f"{SENTINEL_FONT} not resolvable - proves the probe measures presence")
        # The packs are built into the IMAGE (Dockerfile personafonts-* stages), so a
        # bare binary smoke on the build host has none - assert only when mounted.
        if not FONTS_DIR:
            print("  [SKIP] font pack - BROWSER_FONTS_DIR unset (binary smoked without an image)")
        elif SMOKE_PROFILE == "windows":
            expect("Windows font pack", font_state,
                   lambda v: json_ok(v, lambda f: all(f.get(x) is True for x in WINDOWS_CORE_FONTS)),
                   "Arial, Segoe UI, and Calibri present")
        else:
            expect("macOS font pack", font_state,
                   lambda v: json_ok(v, lambda f: all(f.get(x) is True for x in MACOS_CORE_FONTS)),
                   "Helvetica Neue, Helvetica, and Menlo present")
        if FONTS_DIR:
            expect("no substitute leak", font_state,
                   lambda v: json_ok(v, lambda f: not any(
                       f.get(x) is True for x in SUBSTITUTE_SOURCE_FONTS)),
                   "none of " + ", ".join(SUBSTITUTE_SOURCE_FONTS) + " resolvable")
        system_ui_checks()
        webgl_state = cdp_eval("""
            (() => {
              const c = document.createElement('canvas');
              const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
              if (!gl) return {vendor: '', renderer: ''};
              const d = gl.getExtension('WEBGL_debug_renderer_info');
              if (!d) return {vendor: '', renderer: ''};
              return {
                vendor: gl.getParameter(d.UNMASKED_VENDOR_WEBGL),
                renderer: gl.getParameter(d.UNMASKED_RENDERER_WEBGL),
              };
            })()
        """)
        if SMOKE_PROFILE == "macos":
            expect("WebGL = Apple Silicon", webgl_state,
                   lambda v: json_ok(v, lambda g: "Apple" in str(g.get("vendor", ""))
                                     and "Apple M" in str(g.get("renderer", ""))),
                   "Apple vendor + Apple M-series Metal renderer (coherent with architecture=arm)")
        else:
            # The Windows persona had no WebGL assertion at all, so a spoof
            # that collapsed to the software renderer would have shipped
            # unnoticed on the persona that runs on remote hosts.
            expect("WebGL = Windows D3D11", webgl_state,
                   lambda v: json_ok(v, lambda g: "Direct3D11" in str(g.get("renderer", ""))
                                     and not any(bad in str(g.get("renderer", ""))
                                                 for bad in ("SwiftShader", "llvmpipe", "Mesa"))),
                   "a real ANGLE/Direct3D11 adapter, not SwiftShader/llvmpipe/Mesa")
        network_state = cdp_eval("""
            ({
              effectiveType: navigator.connection.effectiveType,
              rtt: navigator.connection.rtt,
              downlink: navigator.connection.downlink,
              saveData: navigator.connection.saveData,
            })
        """)
        expect("navigator.connection datacenter profile", network_state,
               lambda v: json_ok(v, lambda n:
                   isinstance(n, dict) and n.get("effectiveType") == "4g" and
                   isinstance(n.get("rtt"), int) and 10 <= n.get("rtt") <= 65 and
                   isinstance(n.get("downlink"), (int, float)) and 30 <= n.get("downlink") <= 120 and
                   n.get("saveData") is False),
               "4g, rtt 10-65ms, downlink 30-120Mbps, saveData false")
        # Byte for byte the --user-agent the header carries, version included:
        # a marker check passed while patch 0006 hardcoded Chrome/151.
        want_ua = _arg_value(tuple(profile_args), "--user-agent")
        ua = cdp_eval("navigator.userAgent")
        expect(f"UA = {profile['label'].lower()}", ua,
               lambda v: v == json.dumps(want_ua) and profile["ua_marker"] in v,
               json.dumps(want_ua))
        cdp_navigate(trusted_url)
        time.sleep(0.5)
        expect("secure context", cdp_eval("window.isSecureContext"), lambda v: v == "true", "true")
        ua_ch = cdp_eval("""
            (async () => {
              if (!navigator.userAgentData) return null;
              const high = await navigator.userAgentData.getHighEntropyValues(
                ['platform','platformVersion','architecture','bitness','fullVersionList']);
              return {
                platform: high.platform, platformVersion: high.platformVersion,
                architecture: high.architecture, bitness: high.bitness,
                brands: navigator.userAgentData.brands.map(b => b.brand),
                fullBrands: (high.fullVersionList || []).map(b => b.brand),
              };
            })()
        """)
        expect(f"UA-CH = {profile['label'].lower()}/chrome", ua_ch,
               lambda v: json_ok(v, lambda high:
                   isinstance(high, dict) and
                   high.get("platform") == profile["ua_ch_platform"] and
                   high.get("platformVersion") == profile["ua_ch_platform_version"] and
                   high.get("architecture") == profile["architecture"] and
                   high.get("bitness") == "64" and
                   "Google Chrome" in high.get("brands", []) and
                   "Google Chrome" in high.get("fullBrands", []) and
                   GREASE_BRAND in high.get("brands", []) and
                   GREASE_BRAND in high.get("fullBrands", [])),
               f"{profile['label']} + architecture {profile['architecture']} + Google Chrome "
               f"+ GREASE {GREASE_BRAND}")

        # --enable-blink-features=WebShare. navigator.share exists in real
        # Chrome on Windows and macOS desktop but NOT in unbranded Chromium -
        # the same class of gap as the empty speechSynthesis list. Its absence
        # is CreepJS's noWebShare hit, one of only two likeHeadless keys we used
        # to fire that a real Mac does not.
        # --enable-features=WebBluetooth gives navigator.bluetooth; navigator.usb
        # rides along with the same secure-context gating. Real desktop Chrome
        # exposes both, and this gate never looked at either.
        # --use-fake-device-for-media-stream: real machines enumerate at least
        # one audio/video device; an empty list is a headless tell.
        device_apis = cdp_eval("""
            (async () => ({
              share: typeof navigator.share,
              canShare: typeof navigator.canShare,
              bluetooth: typeof navigator.bluetooth,
              usb: typeof navigator.usb,
              devices: (await navigator.mediaDevices.enumerateDevices()).length,
            }))()
        """)
        expect("Chrome-branded + device APIs present", device_apis,
               lambda v: json_ok(v, lambda d:
                   isinstance(d, dict) and
                   d.get("share") == "function" and d.get("canShare") == "function" and
                   d.get("bluetooth") == "object" and d.get("usb") == "object" and
                   d.get("devices", 0) > 0),
               "share/canShare functions, bluetooth+usb objects, >=1 media device")

        # deviceMemory is secure-context gated, so it can only be read here.
        # Production ships it on BOTH personas (AppleSiliconArgs and
        # WindowsMachineArgs each pin it), and real Chrome always exposes it on
        # desktop - "undefined" is a headless tell. Asserting the supplied value
        # also proves the switch survives patch 0050's renderer whitelist.
        expect("deviceMemory", cdp_eval("navigator.deviceMemory"),
               lambda v: v == str(profile["device_memory"]),
               str(profile["device_memory"]))
        display_persona_checks()

        # Patch #53. BarcodeDetector is the single feature separating CreepJS's
        # Windows and Mac platform estimates, so its presence is persona-gated
        # and BOTH directions must be pinned: absent on Windows (real Windows
        # Chrome has no such interface - measured on real hardware), present and
        # fully furnished on macOS. An interface that exists but answers with an
        # empty format list, or rejects detect(), is a stronger tell than one
        # that is simply absent.
        barcode = cdp_eval("""
            (async () => {
              if (!('BarcodeDetector' in window)) return {present: false};
              const formats = await BarcodeDetector.getSupportedFormats();
              const c = document.createElement('canvas');
              c.width = 32; c.height = 32;
              c.getContext('2d').fillRect(0, 0, 32, 32);
              let found = null, err = null;
              try { found = (await new BarcodeDetector().detect(c)).length; }
              catch (e) { err = e.name; }
              return {present: true, formats, found, err};
            })()
        """)
        if profile["barcode_detector"]:
            expect("BarcodeDetector = macOS Vision surface", barcode,
                   lambda v: json_ok(v, lambda b:
                       isinstance(b, dict) and b.get("present") is True and
                       b.get("formats") == MACOS_BARCODE_FORMATS and
                       b.get("found") == 0 and b.get("err") is None),
                   f"{len(MACOS_BARCODE_FORMATS)} Vision formats in order, "
                   "detect() resolves [] rather than rejecting")
        else:
            expect("BarcodeDetector absent", barcode,
                   lambda v: json_ok(v, lambda b:
                       isinstance(b, dict) and b.get("present") is False),
                   "interface not exposed (matches real Windows Chrome)")

        # speechSynthesis. A Chromium build registers no Google network-voice
        # component extension and the container has no speech-dispatcher, so
        # getVoices() answered [] while the persona claimed desktop Chrome.
        # Patch 0052 supplies the persona's list. The first synchronous read must
        # still be empty and the list must arrive by voiceschanged - anti-bot
        # payloads record which of the two produced the list, and how many times
        # the event fired, so the shape is as load-bearing as the contents.
        voices = cdp_eval(VOICES_JS)
        expect(f"speechSynthesis = {profile['label'].lower()} persona", voices,
               lambda v: json_ok(v, lambda s:
                   isinstance(s, dict) and
                   s.get("sync") == 0 and
                   s.get("events") == profile["voices_events"] and
                   s.get("total") == profile["voices_total"] and
                   s.get("local") == profile["voices_local"] and
                   s.get("defCount") == 1 and
                   s.get("def") == profile["voices_default"] and
                   s.get("uriEqName") is True),
               f"empty on first read, {profile['voices_events']} voiceschanged, "
               f"{profile['voices_total']} voices ({profile['voices_local']} local), "
               f"default {profile['voices_default']}, voiceURI == name")

    check_ua_version_and_cpu_tier(profile_args, profile)

    print("\n=== Audio fingerprint differential (seed 1 vs 42069) ===")
    audio_html = (
        "data:text/html,<script>(async()=>{const oc=new OfflineAudioContext(1,5000,44100);"
        "const o=oc.createOscillator();o.type='triangle';o.frequency.value=10000;"
        "const c=oc.createDynamicsCompressor();c.threshold.value=-50;c.knee.value=40;"
        "c.ratio.value=12;c.attack.value=0;c.release.value=0.25;o.connect(c);"
        "c.connect(oc.destination);o.start(0);const b=await oc.startRendering();"
        "const d=b.getChannelData(0);let s=0;for(let i=0;i<d.length;i++)s+=Math.abs(d[i]);"
        "document.title='audio='+s.toFixed(15)})()</script>"
    )
    seeds = []
    for s in ("1", "42069"):
        seed_profile_args, _ = _font_profile_args(s)
        with launch(*seed_profile_args):
            time.sleep(0.5)
            cdp_navigate(audio_html)
            time.sleep(2)
            t = cdp_eval("document.title")
            seeds.append(t)
            print(f"  seed={s} {t}")
    expect("audio FP differs across seeds", str(seeds), lambda v: seeds[0] != seeds[1],
           "two distinct values")
    audio_checks(profile_args)

    canvas_noise_checks()

    # The default-on assertion above cannot cover the opt-out: it asserts the
    # state a switch that never reaches the renderer also produces.
    # --fingerprint-voices shipped inert for exactly that reason - patch 0050
    # whitelists which --fingerprint-* switches are propagated to renderer
    # processes and this one was missing, so the renderer read an empty string
    # and kept the list on. Nothing in a default-on gate can see that. Drive the
    # disable path, and the documented value-required quirk with it.
    print("\n=== speechSynthesis opt-out ===")
    for flag, want, desc in (
        ("--fingerprint-voices=false", 0,
         "no voices - proves the switch reaches the renderer"),
        ("--fingerprint-voices", profile["voices_total"],
         "still on - the value is required, a bare flag is not a disable"),
    ):
        with launch(*profile_args, flag):
            time.sleep(0.5)
            out = cdp_eval(VOICES_JS)
            expect(f"{flag} -> {desc}", out,
                   lambda v, w=want: json_ok(v, lambda s:
                       isinstance(s, dict) and s.get("total") == w),
                   f"total == {want}")

    # The block above supplies explicit screen args because production does.
    # That leaves the seed-default path - which every launch WITHOUT ScreenArgs
    # takes - unobserved, and a gate that only ever drives supplied values
    # cannot see a broken default. Drive it, and re-run the coherence check:
    # whatever the seed picks, screen.* and the media queries must still agree.
    print("\n=== seed-derived screen defaults (no --fingerprint-screen-*) ===")
    with launch(*profile_args):
        time.sleep(0.5)
        default_screen = cdp_eval("""
            ({
              width: screen.width, height: screen.height,
              availWidth: screen.availWidth, availHeight: screen.availHeight,
              mmDevice: matchMedia(
                `(device-width: ${screen.width}px) and (device-height: ${screen.height}px)`
              ).matches,
              mmRes: matchMedia(`(resolution: ${window.devicePixelRatio}dppx)`).matches,
            })
        """)
        expect("seed-default screen is a coherent desktop", default_screen,
               lambda v: json_ok(v, lambda d:
                   isinstance(d, dict) and
                   d.get("width", 0) >= 1024 and d.get("height", 0) >= 600 and
                   d.get("availWidth") == d.get("width") and
                   0 <= d.get("height", 0) - d.get("availHeight", 0) <= 200 and
                   d.get("mmDevice") is True and d.get("mmRes") is True),
               "desktop-sized, availWidth == width, media queries agree")

    check_referrer_and_ua_ch_headers(args, profile)

    webrtc_checks(profile_args)

    print("\n=== driver-shaped: console-preview getter reads with Runtime enabled ===")
    with launch(*profile_args):
        time.sleep(0.5)
        before, after = driver_shaped_getter_reads()
        print(f"  Runtime off: {json.dumps(before, sort_keys=True)}")
        for realm in ("page", "worker"):
            expect(f"{realm} getter reads with Runtime enabled",
                   json.dumps(after.get(realm), sort_keys=True),
                   lambda v, r=realm: isinstance(before.get(r), dict) and
                       before[r].get("errorName") == 1 and
                       json.loads(v) == before[r],
                   f"{json.dumps(before.get(realm), sort_keys=True)}, errorName 1")

    gpu_caps_checks(profile_args)

    if failures:
        print(f"\n{len(failures)} failures:")
        for f in failures:
            print(f"  - {f}")
        return len(failures)
    print("\n[ALL PASSED]")
    return 0


if __name__ == "__main__":
    sys.exit(main())
