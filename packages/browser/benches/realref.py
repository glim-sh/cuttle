#!/usr/bin/env python3
# SPDX: MIT
"""Measure REAL Chrome on this machine, in the shape detect.py emits.

The reference half of the posture checkpoint: run this on a real Mac and a real
Windows box, run detect.py against our binary for the matching persona, and the
four results combine into packages/browser/posture.json.

Headed on purpose. cuttle runs headed under Xvfb, and a headless reference scores
headlessRating 67 where a real browser scores 0 - measuring against it flatters
us on exactly the signals that decide the outcome.

Dependency-free: the raw CDP websocket is hand-rolled because the Windows
reference box has no websocket-client and should not need one.

Usage: realref.py [--json out.json]
"""
from __future__ import annotations

import base64
import json
import os
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

CANDIDATES = [
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
]
PORT = 9614


def find_chrome() -> str:
    for c in CANDIDATES:
        if os.path.exists(c):
            return c
    sys.exit("real Chrome not found")


class Conn:
    def __init__(self, url: str) -> None:
        hp, path = url.split("://", 1)[1].split("/", 1)
        host, port = hp.split(":")
        self.s = socket.create_connection((host, int(port)), timeout=240)
        key = base64.b64encode(os.urandom(16)).decode()
        self.s.sendall((f"GET /{path} HTTP/1.1\r\nHost: {hp}\r\nUpgrade: websocket\r\n"
                        f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
                        f"Sec-WebSocket-Version: 13\r\n\r\n").encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.s.recv(4096)
        self.id = 0

    def _send(self, obj: dict) -> None:
        data = json.dumps(obj).encode()
        mask = os.urandom(4)
        n = len(data)
        hdr = b"\x81" + (bytes([0x80 | n]) if n < 126 else b"\xfe" + struct.pack(">H", n))
        self.s.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

    def _recv(self) -> dict:
        def rd(n: int) -> bytes:
            b = b""
            while len(b) < n:
                b += self.s.recv(n - len(b))
            return b
        _, b2 = rd(2)
        ln = b2 & 127
        if ln == 126:
            ln = struct.unpack(">H", rd(2))[0]
        elif ln == 127:
            ln = struct.unpack(">Q", rd(8))[0]
        return json.loads(rd(ln))

    def cmd(self, method: str, params: dict) -> dict:
        self.id += 1
        mine = self.id
        self._send({"id": mine, "method": method, "params": params})
        while True:
            msg = self._recv()
            if msg.get("id") == mine:
                return msg

    def eval(self, expr: str, await_promise: bool = True):
        r = self.cmd("Runtime.evaluate", {"expression": expr, "returnByValue": True,
                                          "awaitPromise": await_promise})
        return r.get("result", {}).get("result", {}).get("value")


def main() -> int:
    out = None
    if "--json" in sys.argv:
        out = sys.argv[sys.argv.index("--json") + 1]
    chrome = find_chrome()
    profile = tempfile.mkdtemp()
    proc = subprocess.Popen(
        [chrome, f"--remote-debugging-port={PORT}", f"--user-data-dir={profile}",
         "--no-first-run", "--no-default-browser-check", "--remote-allow-origins=*",
         "about:blank"],
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
        sys.exit("real Chrome never exposed a CDP target")
    c = Conn(next(t for t in targets if t["type"] == "page")["webSocketDebuggerUrl"])
    metrics: dict = {"persona": "real", "binary": Path(chrome).name}

    # Secure context: several of these are gated on it.
    c.cmd("Page.navigate", {"url": f"http://127.0.0.1:{PORT}/json/version"})
    time.sleep(2)
    metrics["basics"] = json.loads(c.eval(
        "JSON.stringify({ua: navigator.userAgent, platform: navigator.platform,"
        " hc: navigator.hardwareConcurrency, mem: navigator.deviceMemory,"
        " touch: navigator.maxTouchPoints, langs: navigator.languages,"
        " share: typeof navigator.share, bluetooth: typeof navigator.bluetooth,"
        " usb: typeof navigator.usb, barcode: 'BarcodeDetector' in window,"
        " scrH: screen.height, scrAvailH: screen.availHeight, dpr: devicePixelRatio,"
        # Same three the persona side reports, so the checkpoint compares like
        # with like. On real hardware these are true by definition - which is
        # the point: it records what "agreeing" looks like on a real machine.
        " mmDevice: matchMedia(`(device-width: ${screen.width}px) and"
        " (device-height: ${screen.height}px)`).matches,"
        " mmRes: matchMedia(`(resolution: ${devicePixelRatio}dppx)`).matches,"
        " heapLimitGB: (performance.memory ? +(performance.memory.jsHeapSizeLimit/1073741824).toFixed(2) : null)})"))

    c.cmd("Page.navigate", {"url": "https://abrahamjuliot.github.io/creepjs/"})
    time.sleep(50)
    for _ in range(30):
        if c.eval("typeof window.Fingerprint === 'object' && window.Fingerprint !== null") is True:
            break
        time.sleep(3)
    fp = json.loads(c.eval(
        "JSON.stringify({h: window.Fingerprint.headless || {},"
        " lies: Object.keys((window.Fingerprint.lies||{}).data || {})})") or "{}")
    h = fp.get("h", {})
    metrics["creepjs"] = {
        "headlessRating": h.get("headlessRating"),
        "stealthRating": h.get("stealthRating"),
        "likeHeadlessRating": h.get("likeHeadlessRating"),
        "platformEstimate": (h.get("platformEstimate") or [{}])[0],
        "likeHeadless": sorted(k for k, v in (h.get("likeHeadless") or {}).items() if v),
        "lies": sorted(fp.get("lies", [])),
    }

    try:
        c.cmd("Page.navigate", {"url": "https://deviceandbrowserinfo.com/are_you_a_bot"})
        time.sleep(10)
        txt = c.eval("document.body ? document.body.innerText : ''", False) or ""
        d = json.loads(txt[txt.find("{"):txt.rfind("}") + 1])
        det = d.get("details", d)
        metrics["are_you_a_bot"] = {
            "isBot": d.get("isBot"),
            "flagged": sorted(k for k, v in det.items() if v is True) if isinstance(det, dict) else [],
        }
    except Exception as e:
        metrics["are_you_a_bot"] = {"error": f"{type(e).__name__}"}

    try:
        c.cmd("Page.navigate", {"url": "https://botstop.io/"})
        time.sleep(15)
        txt = (c.eval("document.body ? document.body.innerText : ''", False) or "")
        words = txt.split()
        score = None
        for i, w in enumerate(words):
            if w == "SCORE" and i + 1 < len(words):
                try:
                    score = int(words[i + 1])
                except ValueError:
                    pass
        metrics["botstop"] = {
            "verdict": "HUMAN" if "HUMAN" in txt else ("AUTOMATED" if "AUTOMATED" in txt else "?"),
            "score": score,
        }
    except Exception as e:
        metrics["botstop"] = {"error": f"{type(e).__name__}"}

    proc.terminate()
    print(json.dumps(metrics, indent=2, sort_keys=True))
    if out:
        Path(out).write_text(json.dumps(metrics, indent=2, sort_keys=True) + "\n")
        print(f"\n[realref] metrics -> {out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
