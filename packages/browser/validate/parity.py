#!/usr/bin/env python3
# SPDX: MIT
"""Cross-binary behavioral parity: our amd64 stealth-Chromium vs clark's tarball.

Launches BOTH binaries headless over CDP with an IDENTICAL Windows-persona flag
set and a fixed --fingerprint seed, captures the full fingerprint surface from
each, and asserts every captured vector is byte-identical. Any diff fails with
the offending vector. This is the Phase 2 gate.

Byte-identical BINARIES are impossible (LASTCHANGE/commit-hash stubs); this
proves byte-identical fingerprint SURFACE, which is the thing that matters.

The canvas/rects FARBLING flags are deliberately NOT set here: that noise is
salted by a per-launch session token (independent of --fingerprint), so its
output differs across every process launch - even clark-vs-clark, or ours-vs-
ours. Byte-parity on it is impossible by construction, so asserting it is
meaningless. We instead capture the DETERMINISTIC render (noise off), which
makes byte-equality valid AND stricter: an un-noised compare catches any real
canvas/layout drift (a font or Chromium-version change) that farbling would
otherwise mask. The farbling code path itself is byte-identical to clark by
construction (our patch series is a verbatim fork); that it is active and
seed-responsive is proven separately by the smoke's audio differential.

Env:
  BROWSER_BINARY_PATH   path to our built chrome (required)
  CLARK_REF_PATH        path to clark's chrome (optional; else downloaded)
  BROWSER_FONTS_DIR     Windows fonts dir mounted for both (required for font vector)
Reads versions.env (sibling of packages/browser) for the clark reference URL+sha.
Exit code 0 = zero surface diffs.
"""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
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
FONTS_DIR = (os.environ.get("BROWSER_FONTS_DIR") or "").strip()
SEED = os.environ.get("PARITY_SEED", "42069")

# Identical Windows persona for both binaries (mirrors cuttle ForkParityArgs).
BASE_ARGS = [
    f"--fingerprint={SEED}",
    "--fingerprint-platform=windows",
    "--fingerprint-platform-version=19.0.0",
    "--fingerprint-brand=Chrome",
    "--fingerprint-brand-version=148.0.0.0",
    "--fingerprint-hardware-concurrency=12",
    "--fingerprint-max-touch-points=0",
    "--fingerprint-timezone=America/New_York",
    "--fingerprint-locale=en-US",
    "--fingerprint-network-profile=residential",
    "--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
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


def resolve_clark_ref(workdir: Path) -> Path:
    explicit = os.environ.get("CLARK_REF_PATH")
    if explicit and Path(explicit).exists():
        return Path(explicit)
    url = V.get("CLARK_REF_URL")
    want = V.get("CLARK_REF_SHA256")
    if not url:
        print("ERROR: CLARK_REF_URL missing from versions.env and CLARK_REF_PATH unset", file=sys.stderr)
        sys.exit(2)
    tgz = workdir / "clark-ref.tar.gz"
    print(f"[parity] Downloading clark reference: {url}")
    urllib.request.urlretrieve(url, tgz)
    got = sha256_file(tgz)
    if want and got != want:
        print(f"ERROR: clark ref sha mismatch: got {got}, want {want}", file=sys.stderr)
        sys.exit(2)
    dest = workdir / "clark"
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(tgz) as t:
        t.extractall(dest)
    chrome = next((p for p in dest.rglob("chrome") if p.is_file() and os.access(p, os.X_OK)), None)
    if not chrome:
        chrome = next((p for p in dest.rglob("chrome") if p.is_file()), None)
    if not chrome:
        print("ERROR: no chrome binary in clark reference tarball", file=sys.stderr)
        sys.exit(2)
    return chrome


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


def capture(binary: Path, port: int) -> dict:
    profile = Path(tempfile.mkdtemp(prefix="parity-"))
    cmd = [
        str(binary), "--headless=new", "--no-sandbox", "--use-mock-keychain",
        f"--remote-debugging-port={port}", "--remote-debugging-address=127.0.0.1",
        "--remote-allow-origins=*", f"--user-data-dir={profile}",
        "--disable-gpu", *BASE_ARGS, "about:blank",
    ]
    env = os.environ.copy()
    if FONTS_DIR:
        conf = profile / "fc.conf"
        conf.write_text(
            '<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig>'
            '<include ignore_missing="yes">/etc/fonts/fonts.conf</include>'
            f"<dir>{FONTS_DIR}</dir></fontconfig>"
        )
        env["FONTCONFIG_FILE"] = str(conf)
    proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
    try:
        for _ in range(60):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/version", timeout=1) as r:
                    if r.status == 200:
                        break
            except Exception:
                time.sleep(0.3)
        else:
            raise RuntimeError("CDP never came up")
        time.sleep(0.5)
        return cdp_capture(port)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
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
        clark_bin = resolve_clark_ref(work)
        print(f"[parity] ours = {our_bin}")
        print(f"[parity] clark= {clark_bin}")
        print(f"[parity] seed = {SEED}")
        ours = capture(Path(our_bin), 9455)
        ref = capture(clark_bin, 9456)
        diffs = diff(ours, ref)
        report = HERE / "report.md"
        lines = [f"# Parity report (seed {SEED})", ""]
        if diffs:
            lines.append(f"**{len(diffs)} surface diffs** (FAIL):\n")
            for d in diffs:
                lines.append(f"- `{d}`")
        else:
            lines.append("**Zero surface diffs.** amd64 parity PASS.")
        lines += ["", "## Captured (ours)", "```json", json.dumps(ours, indent=2), "```"]
        report.write_text("\n".join(lines) + "\n")
        print("\n".join(lines[:40]))
        print(f"\n[parity] report -> {report}")
        return 1 if diffs else 0
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
