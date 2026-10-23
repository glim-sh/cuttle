#!/usr/bin/env python3
# SPDX: MIT
"""The browser `cuttle serve` launched, measured through the daemon's own CDP port.

Runs INSIDE the image under test; daemon-gate.sh starts the container and execs
this. smoke.py and detect.py launch Chrome themselves, so neither sees the argv,
profile, display or window the daemon actually gives it.

  daemon-gate.py <windows|macos> pool    [--vnc] [--json out.json]
  daemon-gate.py <windows|macos> session [--vnc]

pool     every detect.py section, attached to the daemon's default-seed browser.
session  the default `cuttle up` deployment, one long-lived headed browser:
         are_you_a_bot with the bundled playwright-cli attached (its
         Runtime.enable is on, as in every driven session), canvas readback and
         the keymap against real Chrome (benches/posture.json), maximized window
         geometry, notification permission, and the keymap again after the
         driver run.
--vnc    also checks that the KasmVNC websocket answers with an RFB handshake.

Exits 1 on any failed assertion. External detector scores, and an external page
that is unreachable, are reported as info.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

os.environ.setdefault("DETECT_CDP_PORT", "9222")
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "benches"))
import detect  # noqa: E402  (reads the persona from sys.argv[1])
import probes  # noqa: E402
import websocket  # noqa: E402

VNC_PORT = 6080
NOTIFICATION_JS = ("(async () => JSON.stringify({secure: isSecureContext,"
                   " permission: typeof Notification === 'undefined' ? 'absent' : Notification.permission,"
                   " query: (await navigator.permissions.query({name: 'notifications'})).state}))()")
# Tab strip plus toolbar of a maximized window on both platforms; posture.json's
# notes record it (the real Windows baseline was captured restored, not maximized).
MAXIMIZED_CHROME_HEIGHT = 87


class _Attached:
    """Stands in for detect.Session's Popen: the daemon owns the browser."""

    def terminate(self) -> None:
        pass

    def wait(self, timeout=None) -> int:
        return 0

    def kill(self) -> None:
        pass


def _attach(self) -> None:
    self.profile = tempfile.mkdtemp()  # Session.close() removes it
    self.proc = _Attached()
    self.ws = None
    self.reconnect()


detect.Session._launch = _attach


def vnc_rfb() -> None:
    """The viewer's websocket must upgrade and speak RFB, or VNC mode is a black tab."""
    status, banner = "101", b""
    try:
        ws = websocket.create_connection(
            f"ws://127.0.0.1:{VNC_PORT}/websockify", subprotocols=["binary"],
            origin=f"http://127.0.0.1:{VNC_PORT}", timeout=10)
        try:
            banner = ws.recv()
        finally:
            ws.close()
    except websocket.WebSocketBadStatusException as e:
        status = str(e.status_code)
    except (OSError, websocket.WebSocketException) as e:
        status = f"{type(e).__name__}: {e}"
    if isinstance(banner, str):
        banner = banner.encode()
    print(f"  VNC websocket: {status}  first frame {banner!r}")
    detect.record("VNC websocket answers RFB", status == "101" and banner.startswith(b"RFB "),
                  f"{status} {banner[:12]!r}")


def driver_are_you_a_bot() -> None:
    """are_you_a_bot over detect.DABI_LOADS loads, driven by the bundled playwright-cli."""
    print("\n=== are_you_a_bot / playwright-cli attached ===")

    def pw(*args: str) -> str:
        try:
            return subprocess.run(["playwright-cli", *args], cwd="/tmp", capture_output=True,
                                  text=True, timeout=120).stdout
        except subprocess.TimeoutExpired:
            return ""

    pw("attach", f"--cdp=http://127.0.0.1:{detect.PORT}")
    bot: list[str] = []
    valid = 0
    try:
        for _ in range(detect.DABI_LOADS):
            pw("goto", detect.DABI_URL)
            time.sleep(10)
            # The driver prints the value as JSON between "### Result" and the next heading.
            result = pw("eval", "() => document.body.innerText").partition("### Result\n")[2]
            try:
                d = detect._json_from_body(json.loads(result.split("\n### ", 1)[0]))
            except (ValueError, AttributeError):
                continue
            if not d:
                continue
            valid += 1
            flagged = ",".join(k for k, v in d.get("details", d).items() if v is True)
            print(f"  load {valid}: isBot {json.dumps(d.get('isBot'))} flagged [{flagged}]")
            # isBot alone, as detect.are_you_a_bot counts it: a lone flagged detail is its noisy timing check.
            if d.get("isBot") is True:
                bot.append(flagged)
    finally:
        pw("detach")
    detect.record(f"are_you_a_bot isBot, driver attached ({len(bot)}/{valid} loads)",
                  not bot if valid else None, "; ".join(bot) if valid else "unavailable")


def keymap(got: dict, real: dict, when: str) -> None:
    want = real["keyboard"]["map"]
    have = (got.get("keyboard") or {}).get("map") or {}
    diff = sorted(k for k in set(want) | set(have) if want.get(k) != have.get(k))
    detect.record(f"keymap matches real Chrome ({when})", not diff,
                  f"{len(have)}/{len(want)} keys" + (f", differ: {', '.join(diff)}" if diff else ""))


def session(s: detect.Session) -> None:
    real = json.loads((Path(detect.__file__).with_name("posture.json")).read_text())
    real = real["platforms"][detect.PERSONA]["real"]["probes"]

    print(f"=== session browser / {detect.PERSONA} persona ===")
    got = probes.run(s, ["canvas", "keyboard", "css"])
    note = json.loads(s.eval(NOTIFICATION_JS, True) or "{}")

    c, rc = got.get("canvas") or {}, real["canvas"]
    print(f"  canvas  stable {c.get('stable')} lowEntropy {c.get('lowEntropyImageData')} toBlob {c.get('toBlobMatches')}")
    canvas_diff = [k for k in ("stable", "lowEntropyImageData", "toBlobMatches") if c.get(k) != rc.get(k)]
    detect.record("canvas readback matches real Chrome",
                  str(c.get("toDataURL", "")).isalnum() and not canvas_diff,
                  c.get("error") or (f"differ: {', '.join(canvas_diff)}" if canvas_diff else ""))

    keymap(got, real, "start")

    g = (got.get("css") or {}).get("screen") or {}
    print(f"  window  {json.dumps(g, sort_keys=True)}")
    want = {"outerWidth": g.get("availWidth"), "outerHeight": g.get("availHeight"),
            "screenX": g.get("availLeft"), "screenY": g.get("availTop"),
            "innerWidth": g.get("outerWidth"),
            "innerHeight": (g.get("outerHeight") or 0) - MAXIMIZED_CHROME_HEIGHT}
    off = [f"{k}={g.get(k)} want {v}" for k, v in want.items() if g.get(k) != v or v is None]
    detect.record("window geometry is a maximized window on the persona screen", not off,
                  "; ".join(off) or f"{g.get('outerWidth')}x{g.get('outerHeight')} on "
                  f"{g.get('width')}x{g.get('height')}, chrome {MAXIMIZED_CHROME_HEIGHT}px")
    try:
        state = s.cmd("Browser.getWindowForTarget", {}).get("result", {}).get("bounds", {}).get("windowState")
    except Exception as e:
        state = f"unavailable ({type(e).__name__})"
    detect.record("X window state", None, str(state))

    print(f"  notifications {note}")
    detect.record("notification permission is stock (secure page)",
                  note == {"secure": True, "permission": "default", "query": "prompt"}, json.dumps(note))

    driver_are_you_a_bot()
    keymap(probes.run(s, ["keyboard"]), real, "after the driver run")


def main() -> int:
    if len(sys.argv) < 3 or sys.argv[2] not in ("pool", "session"):
        sys.exit(__doc__)
    if "--vnc" in sys.argv:
        vnc_rfb()
    if sys.argv[2] == "pool":
        return 1 if detect.main() else 0
    s = detect.Session([])
    try:
        session(s)
    except Exception as e:
        # Not detect.run_section: that records a crash as info, and a gate that
        # passes when its checks never ran is worse than no gate.
        print(f"\n  SESSION CHECKS ABORTED: {type(e).__name__}: {e}")
        detect.record("session checks completed", False, f"{type(e).__name__}: {e}")
    finally:
        s.close()
    return 1 if detect.summary() else 0


if __name__ == "__main__":
    sys.exit(main())
