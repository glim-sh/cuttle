from pathlib import Path
import selkies

root = Path(selkies.__file__).parent
server = root / 'stream_server.py'
text = server.read_text()
anchor = '        self.app.add_routes(routes)'
assert text.count(anchor) == 1, 'Selkies route hook changed'
text = text.replace(anchor, '''        async def cuttle_focus(request):
            import os
            from aiohttp import ClientSession, ClientTimeout
            # Selkies runs without auth here, so only the viewer page itself may
            # hold a stream open (a cross-site no-cors fetch would keep CDP sampling).
            if request.headers.get("Sec-Fetch-Site", "same-origin") != "same-origin":
                raise web.HTTPForbidden()
            response = web.StreamResponse(headers={
                "Content-Type": "text/event-stream", "Cache-Control": "no-store",
                "X-Accel-Buffering": "no"})
            port = os.environ.get("CUTTLE_PORT", "9222")
            async with ClientSession(timeout=ClientTimeout(total=None, sock_read=5)) as client:
                async with client.get(f"http://127.0.0.1:{port}/viewer/focus") as upstream:
                    if upstream.status != 200:
                        raise web.HTTPServiceUnavailable()
                    await response.prepare(request)
                    async for chunk in upstream.content.iter_any():
                        await response.write(chunk)
            return response

        routes.append(web.get(f"{api_prefix}/api/cuttle-focus", cuttle_focus))
''' + anchor)
server.write_text(text)
web = root / 'selkies_web'
index = web / 'index.html'
text = index.read_text()
assert text.count('</body>') == 1, 'Selkies index hook changed'
index.write_text(text.replace('</body>', '<script src="cuttle-keyboard.js" defer></script></body>'))
(web / 'cuttle-keyboard.js').write_bytes(Path(__file__).with_name('keyboard.js').read_bytes())
