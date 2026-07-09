"""EXPERIMENTAL: a browser-only viewer that streams the page over CDP
screencast instead of VNC.

Why it exists: the VNC path (headed Chrome on Xvnc + noVNC) is the working
default, but it carries KasmVNC's forked RFB dialect. CDP screencast is a
dialect-free alternative - Page.startScreencast for frames, Input.dispatch*
for mouse/keyboard, all over the same CDP the agent already uses. Input goes
in as synthesized trusted events (a stealth win, same reason we drive with
compositor-level events elsewhere).

Known limits (why this is experimental, not the default):
- Screencast captures the page VIEWPORT only - not browser chrome, not native
  OS dialogs. Fine for login forms and captchas (page content).
- OAuth sign-in popups are a SEPARATE CDP target; this MVP views one page
  target at a time and does not yet auto-attach to popups. The VNC path shows
  the whole display and handles popups for free - keep it for those flows.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any

import websockets
from aiohttp import WSMsgType, web

from cuttle.cdp import list_targets, pick_page


class Bridge:
    """Fans one CDP screencast out to N browser clients and their input back."""

    def __init__(self) -> None:
        self.clients: set[web.WebSocketResponse] = set()
        self.out: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self.meta: dict[str, Any] = {"deviceWidth": 0, "deviceHeight": 0}

    def send_cdp(self, method: str, params: dict[str, Any] | None = None) -> None:
        self.out.put_nowait({"method": method, "params": params or {}})

    async def broadcast(self, payload: dict[str, Any]) -> None:
        text = json.dumps(payload)
        targets = list(self.clients)
        results = await asyncio.gather(
            *(ws.send_str(text) for ws in targets), return_exceptions=True
        )
        for ws, result in zip(targets, results, strict=True):
            if isinstance(result, Exception):
                self.clients.discard(ws)

    def input_to_cdp(self, msg: dict[str, Any]) -> None:
        """Translate a viewer input event into a CDP Input.* command."""
        kind = msg.get("kind")
        if kind == "mouse":
            dw = self.meta.get("deviceWidth") or 0
            dh = self.meta.get("deviceHeight") or 0
            x = float(msg.get("fx", 0)) * dw
            y = float(msg.get("fy", 0)) * dh
            params: dict[str, Any] = {
                "type": msg["type"],  # mousePressed | mouseReleased | mouseMoved
                "x": x,
                "y": y,
                "button": msg.get("button", "none"),
                "buttons": msg.get("buttons", 0),
                "clickCount": msg.get("clickCount", 0),
                "modifiers": msg.get("modifiers", 0),
            }
            if msg["type"] == "mouseWheel":
                params["deltaX"] = msg.get("deltaX", 0)
                params["deltaY"] = msg.get("deltaY", 0)
            self.send_cdp("Input.dispatchMouseEvent", params)
        elif kind == "key":
            params = {
                "type": msg["type"],  # keyDown | keyUp | char
                "modifiers": msg.get("modifiers", 0),
                "key": msg.get("key", ""),
                "code": msg.get("code", ""),
                "windowsVirtualKeyCode": msg.get("keyCode", 0),
            }
            if msg.get("text"):
                params["text"] = msg["text"]
            self.send_cdp("Input.dispatchKeyEvent", params)


async def _cdp_reader(ws: websockets.ClientConnection, bridge: Bridge) -> None:
    async for raw in ws:
        msg = json.loads(raw)
        if msg.get("method") == "Page.screencastFrame":
            p = msg.get("params", {})
            bridge.meta = p.get("metadata", bridge.meta)
            # Ack before the fanout: Chrome withholds the next frame until the
            # ack, so a slow viewer must not gate the stream for the others.
            bridge.send_cdp("Page.screencastFrameAck", {"sessionId": p.get("sessionId")})
            await bridge.broadcast({"type": "frame", "data": p.get("data", "")})


async def _cdp_writer(ws: websockets.ClientConnection, bridge: Bridge) -> None:
    next_id = 0
    while True:
        cmd = await bridge.out.get()
        next_id += 1
        await ws.send(json.dumps({"id": next_id, **cmd}))


async def _cdp_pump(ws_url: str, bridge: Bridge, target_url: str | None) -> None:
    async with websockets.connect(ws_url, max_size=None) as ws:
        writer = asyncio.create_task(_cdp_writer(ws, bridge))
        bridge.send_cdp("Page.enable")
        if target_url:
            bridge.send_cdp("Page.navigate", {"url": target_url})
        bridge.send_cdp(
            "Page.startScreencast",
            {"format": "jpeg", "quality": 70, "everyNthFrame": 1},
        )
        try:
            await _cdp_reader(ws, bridge)
        finally:
            writer.cancel()


async def _index(_req: web.Request) -> web.Response:
    return web.Response(text=_PAGE, content_type="text/html")


async def _client_ws(req: web.Request) -> web.WebSocketResponse:
    bridge: Bridge = req.app["bridge"]
    ws = web.WebSocketResponse()
    await ws.prepare(req)
    bridge.clients.add(ws)
    try:
        async for msg in ws:
            if msg.type == WSMsgType.TEXT:
                bridge.input_to_cdp(json.loads(msg.data))
            elif msg.type == WSMsgType.ERROR:
                break
    finally:
        bridge.clients.discard(ws)
    return ws


async def serve(cdp_port: int, listen: str, target_url: str | None = None) -> None:
    host, _, port = listen.partition(":")
    target = pick_page(list_targets(cdp_port))
    if not target or not target.get("webSocketDebuggerUrl"):
        raise RuntimeError("no page target found to view")

    bridge = Bridge()
    app = web.Application()
    app["bridge"] = bridge
    app.router.add_get("/", _index)
    app.router.add_get("/ws", _client_ws)

    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, host or "127.0.0.1", int(port))
    await site.start()
    try:
        await _cdp_pump(target["webSocketDebuggerUrl"], bridge, target_url)
    finally:
        await runner.cleanup()


_PAGE = """<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>cuttle view</title>
<style>
  html,body{margin:0;height:100%;background:#000;overflow:hidden}
  #screen{width:100vw;height:100vh;object-fit:contain;display:block}
  #status{position:fixed;top:12px;left:50%;transform:translateX(-50%);
    color:#888;font:13px system-ui;background:#111;padding:4px 12px;border-radius:6px}
</style></head><body>
<img id="screen" alt="">
<div id="status">connecting...</div>
<script>
const img = document.getElementById("screen");
const status = document.getElementById("status");
const proto = location.protocol === "https:" ? "wss:" : "ws:";
let ws;

function modifiers(e){
  return (e.altKey?1:0)|(e.ctrlKey?2:0)|(e.metaKey?4:0)|(e.shiftKey?8:0);
}
function frac(e){
  const r = img.getBoundingClientRect();
  return { fx:(e.clientX-r.left)/r.width, fy:(e.clientY-r.top)/r.height };
}
const BTN = {0:"left",1:"middle",2:"right"};
function send(o){ if(ws && ws.readyState===1) ws.send(JSON.stringify(o)); }

function connect(){
  ws = new WebSocket(proto+"//"+location.host+"/ws");
  ws.onopen = () => { status.style.display="none"; };
  ws.onclose = () => { status.textContent="disconnected - retrying..."; status.style.display="block"; setTimeout(connect, 1500); };
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if(m.type==="frame") img.src = "data:image/jpeg;base64,"+m.data;
  };
}

img.addEventListener("mousemove", e => { const {fx,fy}=frac(e); send({kind:"mouse",type:"mouseMoved",fx,fy,buttons:e.buttons,modifiers:modifiers(e)}); });
img.addEventListener("mousedown", e => { e.preventDefault(); const {fx,fy}=frac(e); send({kind:"mouse",type:"mousePressed",fx,fy,button:BTN[e.button]||"left",buttons:e.buttons,clickCount:1,modifiers:modifiers(e)}); });
img.addEventListener("mouseup", e => { e.preventDefault(); const {fx,fy}=frac(e); send({kind:"mouse",type:"mouseReleased",fx,fy,button:BTN[e.button]||"left",buttons:e.buttons,clickCount:1,modifiers:modifiers(e)}); });
img.addEventListener("contextmenu", e => e.preventDefault());
img.addEventListener("wheel", e => { e.preventDefault(); const {fx,fy}=frac(e); send({kind:"mouse",type:"mouseWheel",fx,fy,deltaX:e.deltaX,deltaY:e.deltaY,modifiers:modifiers(e)}); }, {passive:false});

window.addEventListener("keydown", e => {
  e.preventDefault();
  const text = (e.key.length===1) ? e.key : "";
  send({kind:"key",type:text?"keyDown":"rawKeyDown",key:e.key,code:e.code,keyCode:e.keyCode,text,modifiers:modifiers(e)});
});
window.addEventListener("keyup", e => { e.preventDefault(); send({kind:"key",type:"keyUp",key:e.key,code:e.code,keyCode:e.keyCode,modifiers:modifiers(e)}); });

connect();
</script></body></html>"""
