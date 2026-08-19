# Third-party licenses

cuttle redistributes (or, where noted, optionally builds with) the third-party
software below; their license terms are reproduced in full or linked. Portions of `internal/fingerprint` and `cuttle serve` also
derive from the MIT-licensed `cloakbrowser`/`cloakserve`, used under the MIT
license; no third-party source or binary from them is redistributed.

---

## clark-browser (MIT)

Our baked stealth-Chromium binary (`/opt/browser/chrome`) is built by
`packages/browser` from a patch series that began as clark-browser's
MIT-licensed stealth patches and is now maintained here: rebased onto
ungoogled-chromium 151, with patches added, dropped and authored by us
(`packages/browser/README.md`, "Patch-series contract"). The inherited patches
remain MIT under clark's terms; cuttle-authored ones carry cuttle's.
We do not redistribute clark's prebuilt binary - we redistribute our own build
of their patches. The binary is downloaded and sha256-verified at image build
time from our GitHub release; see `ops/docker/Dockerfile` and `packages/browser/`.

```
MIT License

Copyright (c) 2026 Clark Labs Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

The resulting binary incorporates Chromium (BSD 3-Clause), ungoogled-chromium
(BSD 3-Clause), and Brave-derived farbling code (MPL-2.0); see those upstream
projects for the notices.

---

## KasmVNC (GPL-2.0)

The KasmVNC server (pinned 1.3.3, installed at build time from the project's
GitHub releases as the upstream `.deb`) is baked into the published image and
provides the human-handoff viewer's VNC/WebSocket server. cuttle runs it as a
separate process and does not link against it. Full license text:
https://github.com/kasmtech/KasmVNC/blob/master/LICENSE.TXT

---

## noVNC (MPL-2.0)

The stock noVNC 1.5.0 web client's `core/` and `vendor/` ES modules are baked
unmodified into the image at `/opt/cuttle-www`, fetched at build time from the
project's GitHub tag. Full license text:
https://github.com/novnc/noVNC/blob/master/LICENSE.txt

---

## Windows font pack (OFL 1.1)

The metric-compatible free fonts under `ops/docker/winfonts/` (Liberation,
Carlito, Caladea) are redistributed with their `name` table set to the
corresponding Windows family; see `ops/docker/winfonts/README.md` for the
mapping and provenance. All are licensed under the SIL Open Font License 1.1. No
Microsoft font software is included.
```
SIL OPEN FONT LICENSE Version 1.1 - full text at https://openfontlicense.org
```
