#!/usr/bin/env python3
# SPDX: MIT. Adapted from clark-browser tests/linux_smoke.py and owned here.
"""In-container behavioral smoke test for a built stealth-Chromium binary.

Talks CDP directly (HTTP + WebSocket) via pure-python websocket-client. Asserts
the JS/UA-CH/WebGL/canvas/audio surface against a per-persona expectation set.

cuttle ships exactly two personas, selected by the binary's own arch (see
internal/fingerprint/args.go - personaIsMacOS):
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
# persona active the binary rewrites navigator.userAgent to its own real version
# no matter what --user-agent says, so a stale literal here would assert against
# a UA the binary cannot produce.
CHROMIUM_VERSION = versions_env("CHROMIUM_VERSION")
CHROME_UA_VERSION = CHROMIUM_VERSION.split(".", 1)[0] + ".0.0.0"
# Persona OS versions. Mirror ForkParityArgs in internal/fingerprint/args.go;
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
        "--headless=new", "--no-sandbox", "--use-mock-keychain",
        f"--remote-debugging-port={PORT}",
        "--remote-debugging-address=127.0.0.1",
        "--remote-allow-origins=*",
        f"--user-data-dir={PROFILE}",
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
        return windows_args, {
            "label": "Windows", "navigator_platform": "Win32",
            "ua_marker": "Windows NT 10.0", "ua_ch_platform": "Windows",
            "ua_ch_platform_version": WINDOWS_PLATFORM_VERSION, "architecture": BUILD_ARCH,
            "dpr": 1,
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
            # Real Chrome 151 on macOS 26.7 - the same machine MACOS_PLATFORM_VERSION
            # describes. 180 local plus the 19 Google network voices, one event.
            "voices_total": 199, "voices_local": 180, "voices_events": 1,
            "voices_default": "Samantha",
        }


def main() -> int:
    seed = "42069"
    profile_args, profile = _font_profile_args(seed)
    args = [
        *profile_args,
        "--fingerprint-brand=Chrome",
        f"--fingerprint-brand-version={CHROMIUM_VERSION}",
        "--fingerprint-hardware-concurrency=12",
        "--fingerprint-max-touch-points=0",
        "--fingerprint-timezone=America/New_York",
        "--fingerprint-locale=en-US",
        "--fingerprint-network-profile=datacenter",
        "--accept-lang=en-US,en",
        # Production's value (ForkParityArgs). Chrome accepts one --disable-features,
        # so overriding it here would silently un-fix patch 0040's referrer flips
        # and gate a configuration we never ship.
        "--disable-features=NoReferrers,NoCrossOriginReferrers,MinimalReferrers",
        "--fingerprinting-client-rects-noise",
        "--fingerprinting-canvas-measuretext-noise",
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
        expect("screen/window coherent", screen_state,
               lambda v: json_ok(v, lambda s:
                   isinstance(s, dict) and s.get("width", 0) > 0 and s.get("height", 0) > 0 and
                   s.get("availWidth") == s.get("width") and
                   0 <= s.get("height", 0) - s.get("availHeight", 0) <= 200 and
                   s.get("outerWidth") == s.get("width") and
                   s.get("outerHeight") == s.get("availHeight") and
                   s.get("colorDepth") == 24 and s.get("pixelDepth") == 24 and
                   s.get("devicePixelRatio") == profile["dpr"]),
               "positive desktop screen, matching outer size, 24-bit depth, "
               f"DPR {profile['dpr']}")
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
        # The packs are built into the IMAGE (Dockerfile fontpack stage), so a
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
        if SMOKE_PROFILE == "macos":
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
            expect("WebGL = Apple Silicon", webgl_state,
                   lambda v: json_ok(v, lambda g: "Apple" in str(g.get("vendor", ""))
                                     and "Apple M" in str(g.get("renderer", ""))),
                   "Apple vendor + Apple M-series Metal renderer (coherent with architecture=arm)")
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
        ua = cdp_eval("navigator.userAgent")
        expect(f"UA = {profile['label'].lower()}", ua,
               lambda v: profile["ua_marker"] in v and "HeadlessChrome" not in v,
               f"{profile['ua_marker']} (no Headless)")
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

        # speechSynthesis. A Chromium build registers no Google network-voice
        # component extension and the container has no speech-dispatcher, so
        # getVoices() answered [] while the persona claimed desktop Chrome.
        # Patch 0052 supplies the persona's list. The first synchronous read must
        # still be empty and the list must arrive by voiceschanged - anti-bot
        # payloads record which of the two produced the list, and how many times
        # the event fired, so the shape is as load-bearing as the contents.
        voices = cdp_eval("""
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
        """)
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

    if failures:
        print(f"\n{len(failures)} failures:")
        for f in failures:
            print(f"  - {f}")
        return len(failures)
    print("\n[ALL PASSED]")
    return 0


if __name__ == "__main__":
    sys.exit(main())
