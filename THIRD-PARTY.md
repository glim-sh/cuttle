# Third-party licenses

cuttle derives from and redistributes the following third-party software. Their
license terms are reproduced in full below.

---

## cloakbrowser / cloakserve (MIT)

cuttle's fingerprint argument-builder, proxy normalization, and geoip
(`internal/fingerprint`) are a Go port of CloakHQ's `cloakbrowser` (MIT), and
`cuttle serve` reimplements their `cloakserve` multiplexer. This is authored Go,
not vendored source - no cloakbrowser code and none of CloakBrowser's
proprietary Chromium binary is redistributed. Courtesy attribution to the MIT
upstream the port derives from.

---

## clark-browser (MIT)

Prebuilt stealth-Chromium binary (`/opt/clark/chrome`) baked into the
published image. Downloaded and sha256-verified at build time from the
project's GitHub releases; see `ops/docker/Dockerfile`.

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

clark-browser itself incorporates Chromium (BSD 3-Clause), ungoogled-chromium
(BSD 3-Clause), and Brave-derived farbling code (MPL-2.0); see the upstream
project for those notices.

---

## clearcote-browser (BSD 3-Clause)

Prebuilt stealth-Chromium binary (`/opt/clearcote/chrome`) baked into the
published image as a fallback engine. Downloaded and sha256-verified at build
time from the project's GitHub releases; see `ops/docker/Dockerfile`.

```
BSD 3-Clause License

Copyright (c) 2026, Clearcote Labs and the Clearcote contributors

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

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
