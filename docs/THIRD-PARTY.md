# Third-party licenses

cuttle redistributes (or, where noted, optionally builds with) the third-party
software below; their license terms are reproduced in full or linked. Portions
of `packages/cuttle/internal/fingerprint` and `cuttle serve` also derive from
the MIT-licensed `cloakbrowser`/`cloakserve`, used under the MIT license; no
third-party source or binary from them is redistributed.

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

## playwright-cli and playwright-core (Apache-2.0)

The bundled driver behind `cuttle pw` and `cuttle jev-browse` is Microsoft's
`@playwright/cli` 0.1.20, installed at image build time from npm (the `driver`
stage of `ops/docker/Dockerfile`) together with its two dependencies,
`playwright` and `playwright-core` (1.64.0-alpha-2026-09-14 at that pin). All
three declare `Apache-2.0` in their `package.json`, copyright Microsoft
Corporation. They ship unmodified under `/opt/playwright-cli` in the image, each
with its `LICENSE`; `playwright` and `playwright-core` also carry their `NOTICE`
(Puppeteer-derived code, Apache-2.0) and `ThirdPartyNotices.txt` alongside.
Full license text: https://www.apache.org/licenses/LICENSE-2.0

```
Playwright
Copyright (c) Microsoft Corporation

This software contains code derived from the Puppeteer project (https://github.com/puppeteer/puppeteer),
available under the Apache 2.0 license (https://github.com/puppeteer/puppeteer/blob/master/LICENSE).
```

---

## Node.js (MIT, plus bundled dependencies)

The driver is a Node package, so the image ships the `node` binary from the
official `node:22-trixie-slim` image (`/usr/local/bin/node`; npm is not
copied). Node.js is MIT-licensed; the binary also statically includes V8,
OpenSSL, ICU, libuv, llhttp, zlib and other dependencies under their own
permissive licenses. The image carries the binary only, not Node's `LICENSE`
file; the full text, with every bundled dependency's license, is at
https://github.com/nodejs/node/blob/v22.x/LICENSE

---

## Persona font packs

Each image ships the font pack of its persona under `/opt/personafonts`
(amd64 = Windows, arm64 = macOS). The packs are built at image build time in the
`personafonts-*` stages of `ops/docker/Dockerfile` from Debian trixie font
packages: `scripts/rename-fonts.py` rewrites only the family, full, PostScript
and unique-ID `name` records to the family each font stands in for, and for a
few macOS families stamps Apple's advance widths from
`ops/docker/macfonts/metrics.json`. The fonts' copyright and license records
are left as shipped. No Microsoft or Apple font software is included, and no
renamed font uses its source's Reserved Font Name. The Windows mapping and
provenance are in `ops/docker/winfonts/README.md`.

| Source font (Debian package) | Stands in for | License (Debian `copyright`) |
|---|---|---|
| Liberation Sans/Serif/Mono (`fonts-liberation2`) | Arial, Times New Roman, Courier New; macOS also Times, Courier, Helvetica, Helvetica Neue | SIL OFL 1.1 |
| Carlito (`fonts-crosextra-carlito`) | Calibri, Segoe UI; macOS: Tahoma, Trebuchet MS | SIL OFL 1.1 |
| Caladea (`fonts-crosextra-caladea`) | Cambria; macOS: Georgia | SIL OFL 1.1 |
| Noto Color Emoji (`fonts-noto-color-emoji`) | Segoe UI Emoji; macOS: Apple Color Emoji | SIL OFL 1.1 |
| WenQuanYi Zen Hei (`fonts-wqy-zenhei`) | Microsoft YaHei; macOS: PingFang SC/TC/HK, Hiragino Sans | GPL-2 with font embedding exception, and the M+ FONTS License |
| IPAGothic, IPAPGothic (`fonts-ipafont-gothic`) | MS Gothic, Yu Gothic (Windows only) | IPA Font License 1.0 |
| Loma (`fonts-tlwg-loma-otf`) | Leelawadee UI (Windows only) | GPL-2+ with font exception |
| DejaVu Sans, DejaVu Sans Mono (`fonts-dejavu-core`) | macOS only: Lucida Grande, Geneva, Verdana, Menlo, Monaco | Bitstream Vera Fonts license (DejaVu changes public domain) |

Both packs also carry `cuttle-null`, a glyphless sink font generated by
`scripts/make-null-font.py` (cuttle's own, MIT).

License texts:

- SIL Open Font License 1.1: https://openfontlicense.org
- GPL-2 with the font embedding exception, and the M+ FONTS License:
  https://sources.debian.org/src/fonts-wqy-zenhei/0.9.45-8/debian/copyright/
- IPA Font License 1.0: https://opensource.org/license/ipafont-html
- GPL-2+ with font exception (TLWG):
  https://sources.debian.org/src/fonts-tlwg/1:0.7.3-1/debian/copyright/
- Bitstream Vera Fonts license (DejaVu):
  https://dejavu-fonts.github.io/License.html

---

## Debian packages

The runtime image is `debian:trixie-slim` plus Debian packages (Chromium's
system libraries, Xvfb, openbox, fontconfig, tini, curl and others, listed in
`ops/docker/Dockerfile`). Each is used unmodified under its own license, and
its copyright file ships in the image at `/usr/share/doc/<package>/copyright`.
