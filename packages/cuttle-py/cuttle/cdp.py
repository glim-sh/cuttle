"""Minimal CDP client helpers shared by the CLI and the screencast viewer.

Just enough Chrome DevTools Protocol to list targets, pick the page a human
would want to see, and issue commands - no playwright, no heavy client. Talks
to cuttleserve's HTTP `/json` list and the per-page WebSocket it hands back.
"""

from __future__ import annotations

import json
import urllib.request
from typing import Any

import websockets


def list_targets(cdp_port: int, timeout: float = 5.0) -> list[dict[str, Any]]:
    with urllib.request.urlopen(f"http://127.0.0.1:{cdp_port}/json", timeout=timeout) as resp:
        data = json.loads(resp.read())
    return data if isinstance(data, list) else []


def pick_page(targets: list[dict[str, Any]], vnc_port: int | None = None) -> dict[str, Any] | None:
    """Choose the page target a human is most likely driving.

    Skips workers/service targets, the built-in tab-search UI, and the local
    VNC viewer page itself (so `cuttle view`/`login` never grab their own UI).
    """
    viewer_prefix = f"http://127.0.0.1:{vnc_port}/" if vnc_port else None
    pages = [t for t in targets if t.get("type") == "page"]
    for t in pages:
        url = t.get("url", "")
        if url.startswith("chrome://"):
            continue
        if viewer_prefix and url.startswith(viewer_prefix):
            continue
        return t
    return pages[0] if pages else None


class CDPSession:
    """A single WebSocket connection to one CDP target, with id-matched calls."""

    def __init__(self, ws: websockets.ClientConnection) -> None:
        self._ws = ws
        self._next_id = 0

    async def call(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        self._next_id += 1
        msg_id = self._next_id
        await self._ws.send(json.dumps({"id": msg_id, "method": method, "params": params or {}}))
        while True:
            raw = await self._ws.recv()
            msg = json.loads(raw)
            if msg.get("id") == msg_id:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error'].get('message', msg['error'])}")
                return msg.get("result", {})


async def navigate(cdp_port: int, url: str, vnc_port: int | None = None) -> str:
    """Navigate the active page to `url`; return its title (best-effort)."""
    target = pick_page(list_targets(cdp_port), vnc_port)
    if not target or not target.get("webSocketDebuggerUrl"):
        raise RuntimeError("no page target found to navigate")
    async with websockets.connect(target["webSocketDebuggerUrl"], max_size=None) as ws:
        session = CDPSession(ws)
        await session.call("Page.navigate", {"url": url})
        try:
            res = await session.call(
                "Runtime.evaluate",
                {"expression": "document.title", "returnByValue": True},
            )
            return str(res.get("result", {}).get("value", "") or "")
        except RuntimeError:
            return ""
