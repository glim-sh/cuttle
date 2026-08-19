#!/usr/bin/env python3
# SPDX: MIT
"""Cross-binary behavioral drift: our stealth-Chromium vs our previous release.

Launches BOTH binaries headless over CDP with an IDENTICAL Windows-persona flag
set and a fixed --fingerprint seed, captures the full fingerprint surface from
each, and diffs it. The reference is our own last published release, pinned by
tag and sha256 in versions.env - the clark oracle is retired (dormant at 148, no
151 tarball will ever exist), so the baseline is now our own shipped artifact.

Byte-identical BINARIES are impossible (LASTCHANGE/commit-hash stubs); this
proves a byte-identical fingerprint SURFACE, which is the thing that matters.

Exactly one vector may legitimately differ across a version bump:
navigator.userAgent. The binary stamps its own real version there whenever a
fingerprint persona is active, regardless of --user-agent (measured), so it
cannot be pinned the way every other vector is. It is tolerated ONLY when the
two strings are identical after masking the Chrome/<version> token - a
userAgent diff of any other shape still fails.

The canvas/rects FARBLING flags are deliberately NOT set here: that noise is
salted by a per-launch session token (independent of --fingerprint), so its
output differs across every process launch - even ours-vs-ours. Byte-parity on
it is impossible by construction, so asserting it is meaningless. We instead
capture the DETERMINISTIC render (noise off), which makes byte-equality valid
AND stricter: an un-noised compare catches any real canvas/layout drift (a font
or Chromium-version change) that farbling would otherwise mask. That the
farbling path is active and seed-responsive is proven separately by the smoke's
audio differential.

Env:
  BROWSER_BINARY_PATH   path to our newly built chrome (required)
  BASELINE_REF_PATH     path to the reference chrome (optional; else downloaded)
  BROWSER_FONTS_DIR     persona fonts dir mounted for both (required for font vector)
Reads packages/browser/versions.env for the browser version and the baseline pin.
Exit code 0 = no drift outside the version-derived userAgent.
"""
from __future__ import annotations

import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import threading
import urllib.request
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.sax.saxutils import escape
from pathlib import Path

try:
    import websocket  # type: ignore
except ImportError:
    print("ERROR: pip install websocket-client", file=sys.stderr)
    sys.exit(2)

HERE = Path(__file__).resolve().parent
VERSIONS = HERE.parent / "versions.env"


def load_versions() -> dict[str, str]:
    out: dict[str, str] = {}
    if VERSIONS.exists():
        for line in VERSIONS.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip()
    return out


V = load_versions()
# packages/browser/versions.env is the single source of version truth; the Go
# side pins the same value (internal/fingerprint/args.go chromiumVersion).
CHROMIUM_VERSION = V.get("CHROMIUM_VERSION", "")
if not CHROMIUM_VERSION:
    print(f"ERROR: CHROMIUM_VERSION missing from {VERSIONS}", file=sys.stderr)
    sys.exit(2)
CHROME_UA_VERSION = CHROMIUM_VERSION.split(".", 1)[0] + ".0.0.0"
REPO = "glim-sh/cuttle"
FONTS_DIR = (os.environ.get("BROWSER_FONTS_DIR") or "").strip()
SEED = os.environ.get("PARITY_SEED", "42069")

# Identical Windows persona for both binaries (mirrors cuttle ForkParityArgs).
BASE_ARGS = [
    f"--fingerprint={SEED}",
    "--fingerprint-platform=windows",
    "--fingerprint-platform-version=19.0.0",
    "--fingerprint-brand=Chrome",
    f"--fingerprint-brand-version={CHROMIUM_VERSION}",
    "--fingerprint-hardware-concurrency=12",
    "--fingerprint-max-touch-points=0",
    "--fingerprint-timezone=America/New_York",
    "--fingerprint-locale=en-US",
    "--fingerprint-network-profile=residential",
    "--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    f"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/{CHROME_UA_VERSION} Safari/537.36",
    "--accept-lang=en-US,en",
    # Farbling flags intentionally omitted - see module docstring. Their per-
    # launch salt makes cross-process byte-parity impossible; capturing the
    # deterministic (noise-off) render is both valid and a stricter tripwire.
]
if FONTS_DIR:
    BASE_ARGS.append(f"--fingerprint-fonts-dir={FONTS_DIR}")


def sha256_file(p: Path) -> str:
    h = hashlib.sha256()
    with p.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def resolve_baseline_ref(workdir: Path) -> Path:
    """Our previously published release - the baseline this run diffs against."""
    explicit = os.environ.get("BASELINE_REF_PATH")
    if explicit and Path(explicit).exists():
        return Path(explicit)
    arch = "ARM64" if platform.machine().lower() in ("aarch64", "arm64") else "X64"
    tag = V.get("BROWSER_RELEASE_TAG")
    asset = V.get(f"BROWSER_ASSET_{arch}")
    want = V.get(f"BROWSER_SHA256_{arch}")
    if not tag or not asset:
        print(f"ERROR: BROWSER_RELEASE_TAG/BROWSER_ASSET_{arch} missing from {VERSIONS} "
              "and BASELINE_REF_PATH unset", file=sys.stderr)
        sys.exit(2)
    # The pin is not optional: this tarball is extracted and EXECUTED, so a
    # missing/renamed key must fail rather than silently skip verification.
    if not want:
        print(f"ERROR: BROWSER_SHA256_{arch} missing from {VERSIONS} - refusing to run "
              "an unverified binary", file=sys.stderr)
        sys.exit(2)
    url = f"https://github.com/{REPO}/releases/download/{tag}/{asset}"
    # Cache the reference next to the build cache so repeat runs hash a local file
    # instead of re-downloading ~190MB.
    cache = Path(os.environ.get("BROWSER_WORK_DIR", str(workdir))) / "baseline-ref"
    cache.mkdir(parents=True, exist_ok=True)
    tgz = cache / asset
    if tgz.exists() and sha256_file(tgz) == want:
        print(f"[parity] Using cached baseline: {tgz}")
    else:
        print(f"[parity] Downloading baseline {tag}: {url}")
        urllib.request.urlretrieve(url, tgz)
        got = sha256_file(tgz)
        if got != want:
            print(f"ERROR: baseline sha mismatch: got {got}, want {want}", file=sys.stderr)
            sys.exit(2)
    dest = workdir / "baseline"
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(tgz) as t:
        t.extractall(dest)
    chrome = next((p for p in dest.rglob("chrome") if p.is_file() and os.access(p, os.X_OK)), None)
    if not chrome:
        chrome = next((p for p in dest.rglob("chrome") if p.is_file()), None)
    if not chrome:
        print(f"ERROR: no chrome binary in baseline tarball {asset}", file=sys.stderr)
        sys.exit(2)
    return chrome


def is_version_only(d: str) -> bool:
    """True if a diff is just the two binaries stamping their own version."""
    m = re.fullmatch(r"userAgent: ours=(.*) ref=(.*)", d)
    if not m:
        return False
    def mask(ua: str) -> str:
        return re.sub(r"Chrome/\d+(?:\.\d+){3}", "Chrome/X", ua)

    return mask(m.group(1)) == mask(m.group(2))


CAPTURE_JS = """
(async () => {
  const canvas2d = () => {
    const c = document.createElement('canvas'); c.width = 200; c.height = 50;
    const ctx = c.getContext('2d');
    ctx.textBaseline = 'top'; ctx.font = '14px Arial';
    ctx.fillStyle = '#f60'; ctx.fillRect(0,0,100,20);
    ctx.fillStyle = '#069'; ctx.fillText('stealth-parity', 2, 15);
    return c.toDataURL();
  };
  const webgl = () => {
    const c = document.createElement('canvas');
    const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    if (!gl) return {vendor:'', renderer:''};
    const ext = gl.getExtension('WEBGL_debug_renderer_info');
    return {
      vendor: ext ? gl.getParameter(ext.UNMASKED_VENDOR_WEBGL) : '',
      renderer: ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : '',
      version: gl.getParameter(gl.VERSION),
      shading: gl.getParameter(gl.SHADING_LANGUAGE_VERSION),
    };
  };
  const rects = () => {
    const d = document.createElement('div');
    d.style.cssText = 'position:absolute;left:13.3px;top:7.7px;width:101.9px;height:22.4px';
    document.body.appendChild(d);
    const r = d.getBoundingClientRect();
    return [r.x, r.y, r.width, r.height].map(n => n.toFixed(6)).join(',');
  };
  const uaCH = navigator.userAgentData
    ? await navigator.userAgentData.getHighEntropyValues(
        ['platform','platformVersion','architecture','bitness','model','uaFullVersion','fullVersionList'])
    : null;
  return {
    userAgent: navigator.userAgent,
    platform: navigator.platform,
    hardwareConcurrency: navigator.hardwareConcurrency,
    deviceMemory: navigator.deviceMemory,
    maxTouchPoints: navigator.maxTouchPoints,
    webdriver: navigator.webdriver,
    languages: navigator.languages,
    pluginsLen: navigator.plugins.length,
    plugins: Array.from(navigator.plugins).map(p => p.name),
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    locale: navigator.language,
    screen: {w: screen.width, h: screen.height, aw: screen.availWidth, ah: screen.availHeight,
             cd: screen.colorDepth, dpr: window.devicePixelRatio},
    connection: {et: navigator.connection && navigator.connection.effectiveType,
                 rtt: navigator.connection && navigator.connection.rtt,
                 dl: navigator.connection && navigator.connection.downlink},
    uaCH,
    canvas2d: canvas2d(),
    webgl: webgl(),
    rects: rects(),
    chromeType: typeof window.chrome,
    notif: Notification.permission,
  };
})()
"""


class _TrustedPage(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(b"<!doctype html><title>parity</title>")

    def log_message(self, format: str, *args: object) -> None:
        return


@contextmanager
def trusted_local_page():
    """A local origin both binaries load, so secure-context APIs are populated."""
    server = ThreadingHTTPServer(("127.0.0.1", 0), _TrustedPage)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    host, port = server.server_address
    origin = f"http://{host}:{port}"
    try:
        yield f"{origin}/", origin
    finally:
        server.shutdown()
        server.server_close()


def cdp_navigate(port: int, url: str) -> None:
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=5) as r:
        targets = json.loads(r.read())
    page = next((t for t in targets if t.get("type") == "page"), None)
    if not page:
        return
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=15)
    try:
        ws.send(json.dumps({"id": 1, "method": "Page.navigate", "params": {"url": url}}))
        while True:
            if json.loads(ws.recv()).get("id") == 1:
                break
    finally:
        ws.close()
    time.sleep(1.0)


def cdp_capture(port: int) -> dict:
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=5) as r:
        targets = json.loads(r.read())
    page = next((t for t in targets if t.get("type") == "page"), None)
    if not page:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/new?about:blank", timeout=5) as r:
            page = json.loads(r.read())
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=15)
    try:
        ws.send(json.dumps({"id": 1, "method": "Runtime.evaluate",
                            "params": {"expression": CAPTURE_JS, "returnByValue": True, "awaitPromise": True}}))
        while True:
            msg = json.loads(ws.recv())
            if msg.get("id") == 1:
                if "error" in msg:
                    raise RuntimeError(msg["error"])
                res = msg["result"]["result"]
                if res.get("subtype") == "error" or "value" not in res:
                    raise RuntimeError(f"capture failed: {json.dumps(res)[:300]}")
                return res["value"]
    finally:
        ws.close()


def capture(binary: Path, port: int, trusted: tuple[str, str] | None = None) -> dict:
    profile = Path(tempfile.mkdtemp(prefix="parity-"))
    cmd = [
        str(binary), "--headless=new", "--no-sandbox", "--use-mock-keychain",
        f"--remote-debugging-port={port}", "--remote-debugging-address=127.0.0.1",
        "--remote-allow-origins=*", f"--user-data-dir={profile}",
        *BASE_ARGS,
    ]
    if trusted:
        cmd.append(f"--unsafely-treat-insecure-origin-as-secure={trusted[1]}")
    cmd.append("about:blank")
    env = os.environ.copy()
    if FONTS_DIR:
        conf = profile / "fc.conf"
        conf.write_text(
            '<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig>'
            '<include ignore_missing="yes">/etc/fonts/fonts.conf</include>'
            f"<dir>{escape(FONTS_DIR)}</dir></fontconfig>"
        )
        env["FONTCONFIG_FILE"] = str(conf)
    proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
    try:
        for _ in range(60):
            # A dead child never opens the port; without this the loop burns the
            # full budget and reports a causeless "CDP never came up".
            if proc.poll() is not None:
                raise RuntimeError(f"browser exited before CDP came up (rc={proc.returncode})")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/version", timeout=1) as r:
                    if r.status == 200:
                        break
            except Exception:
                time.sleep(0.3)
        else:
            raise RuntimeError("CDP never came up")
        time.sleep(0.5)
        if trusted:
            cdp_navigate(port, trusted[0])
        return cdp_capture(port)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        shutil.rmtree(profile, ignore_errors=True)


def diff(ours: dict, ref: dict, prefix: str = "") -> list[str]:
    diffs: list[str] = []
    keys = set(ours) | set(ref)
    for k in sorted(keys):
        path = f"{prefix}{k}"
        a, b = ours.get(k), ref.get(k)
        if isinstance(a, dict) and isinstance(b, dict):
            diffs += diff(a, b, path + ".")
        elif a != b:
            diffs.append(f"{path}: ours={a!r} ref={b!r}")
    return diffs


def main() -> int:
    our_bin = os.environ.get("BROWSER_BINARY_PATH")
    if not our_bin or not Path(our_bin).exists():
        print(f"ERROR: BROWSER_BINARY_PATH missing: {our_bin!r}", file=sys.stderr)
        return 2
    if not FONTS_DIR:
        print("[parity] WARN: BROWSER_FONTS_DIR unset; font-dependent vectors skipped")
    work = Path(tempfile.mkdtemp(prefix="parity-work-"))
    try:
        ref_bin = resolve_baseline_ref(work)
        print(f"[parity] ours     = {our_bin}")
        print(f"[parity] baseline = {ref_bin}")
        print(f"[parity] seed     = {SEED}")
        with trusted_local_page() as trusted:
            ours = capture(Path(our_bin), 9455, trusted)
            ref = capture(ref_bin, 9456, trusted)
        diffs = diff(ours, ref)
        expected = [d for d in diffs if is_version_only(d)]
        drift = [d for d in diffs if d not in expected]
        report = Path(os.environ.get("PARITY_REPORT", HERE / "report.md"))
        baseline = os.environ.get("BASELINE_REF_PATH") or V.get("BROWSER_RELEASE_TAG", "?")
        lines = [f"# Surface drift report (seed {SEED})", "",
                 f"Baseline: `{baseline}`  ->  ours: `{CHROMIUM_VERSION}`", ""]
        if drift:
            lines.append(f"**{len(drift)} unexplained surface diffs** (FAIL):\n")
            lines += [f"- `{d}`" for d in drift]
        else:
            lines.append("**Zero unexplained surface diffs.** PASS.")
        if expected:
            lines += ["", "Version-derived (expected across a browser bump):", ""]
            lines += [f"- `{d}`" for d in expected]
        lines += ["", "## Captured (ours)", "```json", json.dumps(ours, indent=2), "```"]
        report.write_text("\n".join(lines) + "\n")
        print("\n".join(lines[:40]))
        print(f"\n[parity] report -> {report}")
        return 1 if drift else 0
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
